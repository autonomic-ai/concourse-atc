package main

import (
	"crypto/rsa"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io/ioutil"
	"net"
	"net/http"
	_ "net/http/pprof"
	"net/url"
	"os"
	"strings"
	"time"

	httpmetrics "github.com/codahale/http-handlers/metrics"
	_ "github.com/codahale/metrics/runtime"
	"github.com/dgrijalva/jwt-go"
	"github.com/felixge/tcpkeepalive"
	"github.com/gorilla/context"
	"github.com/lib/pq"
	"github.com/nu7hatch/gouuid"
	"github.com/pivotal-golang/clock"
	"github.com/pivotal-golang/lager"
	"github.com/tedsuo/ifrit"
	"github.com/tedsuo/ifrit/grouper"
	"github.com/tedsuo/ifrit/http_server"
	"github.com/tedsuo/ifrit/sigmon"
	"github.com/tedsuo/rata"
	"github.com/xoebus/zest"

	"github.com/concourse/atc"
	"github.com/concourse/atc/api"
	"github.com/concourse/atc/api/buildserver"
	"github.com/concourse/atc/auth"
	"github.com/concourse/atc/auth/github"
	"github.com/concourse/atc/builds"
	"github.com/concourse/atc/config"
	Db "github.com/concourse/atc/db"
	"github.com/concourse/atc/db/migrations"
	"github.com/concourse/atc/engine"
	"github.com/concourse/atc/exec"
	"github.com/concourse/atc/metric"
	"github.com/concourse/atc/pipelines"
	rdr "github.com/concourse/atc/radar"
	"github.com/concourse/atc/resource"
	sched "github.com/concourse/atc/scheduler"
	"github.com/concourse/atc/web/webhandler"
	"github.com/concourse/atc/worker"
	"github.com/concourse/atc/wrappa"
)

var pipelinePath = flag.String(
	"pipeline",
	"",
	"path to atc pipeline config .yml",
)

var templatesDir = flag.String(
	"templates",
	"./web/templates",
	"path to directory containing the html templates",
)

var publicDir = flag.String(
	"public",
	"./web/public",
	"path to directory containing public resources (javascript, css, etc.)",
)

var gardenNetwork = flag.String(
	"gardenNetwork",
	"",
	"garden API network type (tcp/unix). leave empty for dynamic registration.",
)

var gardenAddr = flag.String(
	"gardenAddr",
	"",
	"garden API network address (host:port or socket path). leave empty for dynamic registration.",
)

var baggageclaimURL = flag.String(
	"baggageclaimURL",
	"",
	"baggageclaim API endpoint. leave empty for dynamic registration.",
)

var resourceTypes = flag.String(
	"resourceTypes",
	`[
		{"type": "archive", "image": "docker:///concourse/archive-resource" },
		{"type": "docker-image", "image": "docker:///concourse/docker-image-resource" },
		{"type": "git", "image": "docker:///concourse/git-resource" },
		{"type": "github-release", "image": "docker:///concourse/github-release-resource" },
		{"type": "s3", "image": "docker:///concourse/s3-resource" },
		{"type": "semver", "image": "docker:///concourse/semver-resource" },
		{"type": "time", "image": "docker:///concourse/time-resource" },
		{"type": "tracker", "image": "docker:///concourse/tracker-resource" },
		{"type": "pool", "image": "docker:///concourse/pool-resource" }
	]`,
	"map of resource type to its rootfs",
)

var sqlDriver = flag.String(
	"sqlDriver",
	"postgres",
	"database/sql driver name",
)

var sqlDataSource = flag.String(
	"sqlDataSource",
	"postgres://127.0.0.1:5432/atc?sslmode=disable",
	"database/sql data source configuration string",
)

var webListenAddress = flag.String(
	"webListenAddress",
	"0.0.0.0",
	"address to listen on",
)

var webListenPort = flag.Int(
	"webListenPort",
	8080,
	"port for the web server to listen on",
)

var callbacksURLString = flag.String(
	"callbacksURL",
	"http://127.0.0.1:8080",
	"URL used for callbacks to reach the ATC (excluding basic auth)",
)

var debugListenAddress = flag.String(
	"debugListenAddress",
	"127.0.0.1",
	"address for the pprof debugger listen on",
)

var debugListenPort = flag.Int(
	"debugListenPort",
	8079,
	"port for the pprof debugger to listen on",
)

var httpUsername = flag.String(
	"httpUsername",
	"",
	"basic auth username for the server",
)

var httpPassword = flag.String(
	"httpPassword",
	"",
	"basic auth password for the server",
)

var httpHashedPassword = flag.String(
	"httpHashedPassword",
	"",
	"bcrypted basic auth password for the server",
)

var gitHubAuthClientID = flag.String(
	"gitHubAuthClientID",
	"",
	"client ID to use for enabling github auth",
)

var gitHubAuthClientSecret = flag.String(
	"gitHubAuthClientSecret",
	"",
	"client secret to use for enabling github auth",
)

var gitHubAuthOrg = flag.String(
	"gitHubAuthOrg",
	"",
	"name of github organization a user must be a member of to be authenticated",
)

var checkInterval = flag.Duration(
	"checkInterval",
	1*time.Minute,
	"interval on which to poll for new versions of resources",
)

var publiclyViewable = flag.Bool(
	"publiclyViewable",
	false,
	"allow viewability without authentication (destructive operations still require auth)",
)

var externalURL = flag.String(
	"externalURL",
	"",
	"allow viewability without authentication (destructive operations still require auth)",
)

var dev = flag.Bool(
	"dev",
	false,
	"dev mode; lax security",
)

var noop = flag.Bool(
	"noop",
	false,
	"don't trigger any builds automatically",
)

var cliDownloadsDir = flag.String(
	"cliDownloadsDir",
	"",
	"directory containing CLI binaries to serve",
)

var yellerAPIKey = flag.String(
	"yellerAPIKey",
	"",
	"API token to output error logs to Yeller",
)

var yellerEnvironment = flag.String(
	"yellerEnvironment",
	"development",
	"environment label for Yeller",
)

var riemannAddr = flag.String(
	"riemannAddr",
	"",
	"Address of a Riemann server to emit metrics to.",
)

var riemannHost = flag.String(
	"riemannHost",
	"",
	"Host name to associate with metrics emitted.",
)

var riemannTags = flag.String(
	"riemannTags",
	"",
	"Comma-separated list of tags to attach to all emitted metrics, e.g. tag-1,tag-2.",
)

var riemannAttributes = flag.String(
	"riemannAttributes",
	"",
	"Comma-separated list of key-value pairs to attach to all emitted metrics, e.g. a=b,c=d.",
)

var sessionSigningKeyFile = flag.String(
	"sessionSigningKeyFile",
	"",
	"file containing an RSA private key to use when signing session",
)

func main() {
	flag.Parse()

	if !*dev && (*httpUsername == "" || (*httpHashedPassword == "" && *httpPassword == "")) && *gitHubAuthOrg == "" {
		fatal(errors.New("must specify -httpUsername and -httpPassword or -httpHashedPassword, or -githubAuthOrg, or turn on dev mode"))
	}

	if *gitHubAuthClientID != "" && *gitHubAuthClientSecret != "" && *externalURL == "" {
		fatal(errors.New("must specify -externalURL if -gitHubAuthClientID and -gitHubAuthClientSecret are given"))
	}

	if *gitHubAuthClientID != "" && *gitHubAuthClientSecret != "" && *sessionSigningKeyFile == "" {
		fatal(errors.New("must specify -sessionSigningKeyFile if -gitHubAuthClientID and -gitHubAuthClientSecret are given"))
	}

	if _, err := os.Stat(*templatesDir); err != nil {
		fatal(errors.New("directory specified via -templates does not exist"))
	}

	if _, err := os.Stat(*publicDir); err != nil {
		fatal(errors.New("directory specified via -public does not exist"))
	}

	logger := lager.NewLogger("atc")

	logLevel := lager.INFO
	if *dev {
		logLevel = lager.DEBUG
	}

	sink := lager.NewReconfigurableSink(lager.NewWriterSink(os.Stdout, lager.DEBUG), logLevel)
	logger.RegisterSink(sink)

	if *yellerAPIKey != "" {
		yellerSink := zest.NewYellerSink(*yellerAPIKey, *yellerEnvironment)
		logger.RegisterSink(yellerSink)
	}

	if *riemannAddr != "" {
		host := *riemannHost
		if host == "" {
			host, _ = os.Hostname()
		}

		metric.Initialize(
			logger.Session("metrics"),
			*riemannAddr,
			host,
			strings.Split(*riemannTags, ","),
			parseAttributes(logger, *riemannAttributes),
		)
	}

	var err error

	var dbConn Db.Conn
	dbConn, err = migrations.LockDBAndMigrate(logger.Session("db.migrations"), *sqlDriver, *sqlDataSource)
	if err != nil {
		panic("could not lock db and migrate: " + err.Error())
	}

	listener := pq.NewListener(*sqlDataSource, time.Second, time.Minute, nil)
	bus := Db.NewNotificationsBus(listener, dbConn)

	explainDBConn := Db.Explain(logger, dbConn, clock.NewClock(), 500*time.Millisecond)
	db := Db.NewSQL(logger.Session("db"), explainDBConn, bus)
	pipelineDBFactory := Db.NewPipelineDBFactory(logger.Session("db"), explainDBConn, bus, db)

	var configDB Db.ConfigDB
	configDB = Db.PlanConvertingConfigDB{NestedDB: db}

	var resourceTypesNG []atc.WorkerResourceType
	err = json.Unmarshal([]byte(*resourceTypes), &resourceTypesNG)
	if err != nil {
		logger.Fatal("invalid-resource-types", err)
	}

	workerClient := worker.NewPool(
		worker.NewDBWorkerProvider(
			logger,
			db,
			keepaliveDialer,
			worker.ExponentialRetryPolicy{
				Timeout: 5 * time.Minute,
			},
		),
	)

	tracker := resource.NewTracker(workerClient)

	gardenFactory := exec.NewGardenFactory(workerClient, tracker, func() string {
		guid, err := uuid.NewV4()
		if err != nil {
			panic("not enough entropy to generate guid: " + err.Error())
		}

		return guid.String()
	})

	execEngine := engine.NewExecEngine(gardenFactory, engine.NewBuildDelegateFactory(db), db)

	engine := engine.NewDBEngine(engine.Engines{execEngine}, db)

	var signingKey *rsa.PrivateKey

	if *sessionSigningKeyFile != "" {
		rsaKeyBlob, err := ioutil.ReadFile(*sessionSigningKeyFile)
		if err != nil {
			fatal(err)
		}

		signingKey, err = jwt.ParseRSAPrivateKeyFromPEM(rsaKeyBlob)
		if err != nil {
			fatal(err)
		}
	}

	validator, basicAuthEnabled := constructValidator(signingKey)

	oauthProviders := auth.Providers{}

	if *gitHubAuthOrg != "" {
		path, err := auth.OAuthRoutes.CreatePathForRoute(auth.OAuthCallback, rata.Params{
			"provider": github.ProviderName,
		})
		if err != nil {
			fatal(err)
		}

		oauthProviders[github.ProviderName] = github.NewProvider(
			*gitHubAuthOrg,
			*gitHubAuthClientID,
			*gitHubAuthClientSecret,
			*externalURL+path,
		)
	}

	callbacksURL, err := url.Parse(*callbacksURLString)
	if err != nil {
		fatal(err)
	}

	drain := make(chan struct{})

	apiHandler, err := api.NewHandler(
		logger,
		validator,
		pipelineDBFactory,

		configDB,

		db, // buildserver.BuildsDB
		db, // workerserver.WorkerDB
		db, // containerServer.ContainerDB
		db, // pipes.PipeDB
		db, // db.PipelinesDB

		config.ValidateConfig,
		callbacksURL.String(),
		buildserver.NewEventHandler,
		drain,

		engine,
		workerClient,

		sink,

		*cliDownloadsDir,
	)
	if err != nil {
		fatal(err)
	}

	oauthHandler, err := auth.NewOAuthHandler(
		logger,
		oauthProviders,
		signingKey,
	)
	if err != nil {
		fatal(err)
	}

	radarSchedulerFactory := pipelines.NewRadarSchedulerFactory(
		tracker,
		*checkInterval,
		engine,
		db,
	)

	authWrapper := wrappa.NewWebAuthWrappa(*publiclyViewable, validator)

	webHandler, err := webhandler.NewHandler(
		logger,
		authWrapper,
		oauthProviders,
		basicAuthEnabled,
		radarSchedulerFactory,
		db,
		pipelineDBFactory,
		configDB,
		*templatesDir,
		*publicDir,
		engine,
	)
	if err != nil {
		fatal(err)
	}

	webMux := http.NewServeMux()
	webMux.Handle("/api/v1/", apiHandler)
	webMux.Handle("/auth/", oauthHandler)
	webMux.Handle("/", webHandler)

	var httpHandler http.Handler

	httpHandler = webMux

	httpHandler = auth.CookieSetHandler{
		Handler: httpHandler,
	}

	httpHandler = httpmetrics.Wrap(httpHandler)

	// avoid leaking per-request context
	httpHandler = context.ClearHandler(httpHandler)

	webListenAddr := fmt.Sprintf("%s:%d", *webListenAddress, *webListenPort)
	debugListenAddr := fmt.Sprintf("%s:%d", *debugListenAddress, *debugListenPort)

	syncer := pipelines.NewSyncer(
		logger.Session("syncer"),
		db,
		pipelineDBFactory,
		func(pipelineDB Db.PipelineDB) ifrit.Runner {
			return grouper.NewParallel(os.Interrupt, grouper.Members{
				{
					pipelineDB.ScopedName("radar"),
					rdr.NewRunner(
						logger.Session(pipelineDB.ScopedName("radar")),
						*noop,
						radarSchedulerFactory.BuildRadar(pipelineDB),
						pipelineDB,
						1*time.Minute,
					),
				},
				{
					pipelineDB.ScopedName("scheduler"),
					&sched.Runner{
						Logger: logger.Session(pipelineDB.ScopedName("scheduler")),

						DB: pipelineDB,

						Scheduler: radarSchedulerFactory.BuildScheduler(pipelineDB),

						Noop: *noop,

						Interval: 10 * time.Second,
					},
				},
			})
		},
	)

	buildTracker := builds.NewTracker(
		logger.Session("build-tracker"),
		db,
		engine,
	)

	memberGrouper := []grouper.Member{
		{"web", http_server.New(webListenAddr, httpHandler)},

		{"debug", http_server.New(debugListenAddr, http.DefaultServeMux)},

		{"drainer", ifrit.RunFunc(func(signals <-chan os.Signal, ready chan<- struct{}) error {
			close(ready)

			<-signals

			close(drain)

			return nil
		})},

		{"pipelines", pipelines.SyncRunner{
			Syncer:   syncer,
			Interval: 10 * time.Second,
			Clock:    clock.NewClock(),
		}},

		{"builds", builds.TrackerRunner{
			Tracker:  buildTracker,
			Interval: 10 * time.Second,
			Clock:    clock.NewClock(),
		}},
	}

	// register a hardcoded worker
	if *gardenAddr != "" {
		memberGrouper = append(memberGrouper,
			grouper.Member{
				Name: "hardcoded-worker",
				Runner: worker.NewHardcoded(
					logger, db, clock.NewClock(),
					*gardenAddr, *baggageclaimURL, resourceTypesNG,
				),
			},
		)
	}

	group := grouper.NewParallel(os.Interrupt, memberGrouper)

	running := ifrit.Invoke(sigmon.New(group))

	logger.Info("listening", lager.Data{
		"web":   webListenAddr,
		"debug": debugListenAddr,
	})

	err = <-running.Wait()
	if err != nil {
		logger.Error("exited-with-failure", err)
		os.Exit(1)
	}
}

func fatal(err error) {
	println(err.Error())
	os.Exit(1)
}

func constructValidator(signingKey *rsa.PrivateKey) (auth.Validator, bool) {
	if *dev {
		return auth.NoopValidator{}, false
	}

	var basicAuthValidator auth.Validator

	if *httpUsername != "" && *httpHashedPassword != "" {
		basicAuthValidator = auth.BasicAuthHashedValidator{
			Username:       *httpUsername,
			HashedPassword: *httpHashedPassword,
		}
	} else if *httpUsername != "" && *httpPassword != "" {
		basicAuthValidator = auth.BasicAuthValidator{
			Username: *httpUsername,
			Password: *httpPassword,
		}
	}

	var jwtValidator auth.Validator

	if signingKey != nil {
		jwtValidator = auth.JWTValidator{
			PublicKey: &signingKey.PublicKey,
		}
	}

	var validator auth.Validator

	if basicAuthValidator != nil && jwtValidator != nil {
		validator = auth.ValidatorBasket{basicAuthValidator, jwtValidator}
	} else if basicAuthValidator != nil {
		validator = basicAuthValidator
	} else if jwtValidator != nil {
		validator = jwtValidator
	} else {
		validator = auth.NoopValidator{}
	}

	return validator, basicAuthValidator != nil
}

func parseAttributes(logger lager.Logger, pairs string) map[string]string {
	attributes := map[string]string{}
	for _, pair := range strings.Split(*riemannAttributes, ",") {
		segs := strings.SplitN(pair, "=", 2)
		if len(segs) != 2 {
			logger.Fatal("malformed-key-value-pair", nil, lager.Data{"pair": pair})
		}

		attributes[segs[0]] = attributes[segs[1]]
	}

	return attributes
}

func keepaliveDialer(network string, address string) (net.Conn, error) {
	conn, err := net.DialTimeout(network, address, 5*time.Second)
	if err != nil {
		return nil, err
	}

	err = tcpkeepalive.SetKeepAlive(conn, 10*time.Second, 3, 5*time.Second)
	if err != nil {
		println("failed to enable connection keepalive: " + err.Error())
	}

	return conn, nil
}
