package server

import (
	"context"
	"errors"
	"file-service/internal/files"
	"file-service/pkg/filelogger"
	"file-service/pkg/queuelogger"
	serverlogging "file-service/pkg/serverlogging/gin"
	"fmt"
	"github.com/gin-gonic/gin"
	"github.com/ilyakaznacheev/cleanenv"
	configService "github.com/llc-ldbit/go-cloud-config-client"
	amqp "github.com/rabbitmq/amqp091-go"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"
)

func Run() {
	log.Println("starting service...")
	ctx := context.Background()

	// read config from env
	var cfg Config
	if err := cleanenv.ReadEnv(&cfg); err != nil {
		log.Fatalf("failed to load env config: %s", err)
	}

	// setup config service
	cfgService, err := configService.NewConfigServiceManager(cfg.ServiceName, cfg.ConfigServiceUrl(),
		time.Second*time.Duration(cfg.ConfigUpdateInterval))
	if err != nil {
		log.Fatalln("failed to init setting service:", err)
	}
	if err := cfgService.FillConfigStruct(&cfg); err != nil {
		log.Fatalln("failed to fill config from config service:", err)
	}

	// setup queue logger
	mqLogger, err := queuelogger.NewRemoteLogger(cfg.MqUrl(), cfg.LogQueue, cfg.ServiceName)
	if err != nil {
		log.Fatalf("fail to initialize mqLogger cause %v", err)
	}
	defer mqLogger.Close()

	// setting up fileLogger
	fileLogger := filelogger.NewFileLogger("app.log")
	fileLogger.EnableConsoleLog()

	// connect to rabbitmq
	log.Println("connecting to rabbitmq", cfg.MqUrl())
	mqConn, err := amqp.Dial(cfg.MqUrl())
	if err != nil {
		log.Fatalln("failed to connect to rabbitmq:", err)
	}
	defer mqConn.Close()

	// create service
	filesService, err := files.NewService()
	if err != nil {
		log.Fatalln("failed to create service:", err)
	}

	// create consumer
	consumer, err := files.NewConsumer(filesService, mqConn, cfg.FileQueue, fileLogger, mqLogger)
	if err != nil {
		log.Fatalln("failed to create consumer:", err)
	}

	// start consumer
	if err := consumer.Start(); err != nil {
		log.Fatalln("failed to start consumer:", err)
	}

	// setting up gin app
	gin.SetMode(gin.ReleaseMode)
	app := gin.New()
	app.Use(gin.CustomRecovery(serverlogging.NewPanicLogger(fileLogger, mqLogger)))
	app.Use(serverlogging.NewRequestLogger(fileLogger, mqLogger))

	// init handlers
	apiV1 := app.Group("/api/v1/files")

	// admin handler
	files.RegisterAdminHandler(apiV1.Group("/admin"))

	// user exists handler
	files.RegisterFileHandler(apiV1.Group("/id"), filesService)

	// setting up server
	srv := &http.Server{
		Addr:    fmt.Sprintf("%s:%s", cfg.Host, cfg.Port),
		Handler: app,
	}

	// start server
	go func() {
		log.Println("listening at ", fmt.Sprintf("%s:%s", cfg.Host, cfg.Port))
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Fatalf("listen and serve error: %s\n", err)
		}
	}()

	// graceful shutdown
	signalCh := make(chan os.Signal, 1)
	signal.Notify(signalCh, syscall.SIGINT, syscall.SIGTERM)
	<-signalCh
	log.Println("Graceful shutdown, timeout of 5 seconds ...")
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	consumer.Close()
	if err := srv.Shutdown(ctx); err != nil {
		log.Fatal("Server Shutdown error:", err)
	}
	<-ctx.Done()
	log.Println("Timeout of 5 seconds done, exiting")
}
