package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math/rand"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/podpivasniki1488/assyl-backend/internal/delivery"
	"github.com/podpivasniki1488/assyl-backend/internal/repository"
	"github.com/podpivasniki1488/assyl-backend/internal/service"
	"github.com/redis/go-redis/v9"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracegrpc"
	"go.opentelemetry.io/otel/exporters/stdout/stdoutlog"
	"go.opentelemetry.io/otel/exporters/stdout/stdoutmetric"
	"go.opentelemetry.io/otel/log/global"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/sdk/log"
	"go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	semconv "go.opentelemetry.io/otel/semconv/v1.17.0"
)

// Package main Assyl Backend API.
//
// @title           Assyl Backend API
// @version         1.0
// @description     API для работы с приложением для ЖК.
//
// @host      localhost:8080
// @BasePath  /api/v1
func main() {
	// get some envs somewhere
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	//TODO: FINALLY NEED TO DEPLOY AND START IN HEROKU

	defer func() {
		if err := recover(); err != nil {
			fmt.Println("panic:", err)
			// TODO: maybe sentry?
		}
	}()

	rand.NewSource(time.Now().UnixNano())

	cfg := mustReadConfig()

	shutdownOTel, err := setupOTelSDK(ctx)
	if err != nil {
		panic(err)
	}
	defer func() {
		_ = shutdownOTel(ctx)
	}()

	tracer := otel.Tracer("global")

	rdb := redis.NewClient(&redis.Options{
		Addr:     cfg.RedisDSN,
		Username: cfg.RedisUsername,
		Password: cfg.RedisPassword,
		DB:       0,
	})

	txtHandler := slog.NewTextHandler(os.Stdin, nil)
	logger := slog.New(txtHandler)

	debug := cfg.Debug //TODO: debug from env

	db := repository.MustInitDb(cfg.DBDSN)

	repo := repository.NewRepository(db, debug, tracer)

	srv := service.NewService(repo, tracer, rdb)

	d := delivery.NewDelivery(logger, srv, tracer)

	port := os.Getenv("PORT")
	if port == "" {
		panic("PORT environment variable not set")
	}

	go func() {
		d.Http.Start(":" + port)
	}()

	sigs := make(chan os.Signal, 1)

	signal.Notify(sigs, syscall.SIGINT, syscall.SIGTERM)
	<-sigs

	shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer shutdownCancel()
	d.Http.Stop(shutdownCtx)
}

func mustReadConfig() Config {
	cfg := Config{
		RedisDSN:      os.Getenv("REDIS_DSN"),
		RedisUsername: os.Getenv("REDIS_USERNAME"),
		RedisPassword: os.Getenv("REDIS_PASSWORD"),
		JwtSecretKey:  os.Getenv("JWT_SECRET"),
		DBDSN:         os.Getenv("DB_DSN"),
		Debug:         os.Getenv("DEBUG") == "true",
		GmailUsername: os.Getenv("GMAIL_USERNAME"),
		GmailPassword: os.Getenv("GMAIL_PASSWORD"),
		HttpPort:      os.Getenv("PORT"),
	}

	return cfg
}

type Config struct {
	RedisDSN      string
	RedisUsername string
	RedisPassword string
	DBDSN         string
	JwtSecretKey  string
	Debug         bool
	GmailUsername string
	GmailPassword string
	HttpPort      string
}

// setupOTelSDK bootstraps the OpenTelemetry pipeline.
// If it does not return an error, make sure to call shutdown for proper cleanup.
func setupOTelSDK(ctx context.Context) (func(context.Context) error, error) {
	var shutdownFuncs []func(context.Context) error
	var err error

	// shutdown calls cleanup functions registered via shutdownFuncs.
	// The errors from the calls are joined.
	// Each registered cleanup will be invoked once.
	shutdown := func(ctx context.Context) error {
		var err error
		for _, fn := range shutdownFuncs {
			err = errors.Join(err, fn(ctx))
		}
		shutdownFuncs = nil
		return err
	}

	// handleErr calls shutdown for cleanup and makes sure that all errors are returned.
	handleErr := func(inErr error) {
		err = errors.Join(inErr, shutdown(ctx))
	}

	// Set up propagator.
	prop := newPropagator()
	otel.SetTextMapPropagator(prop)

	// Set up trace provider.
	tracerProvider, err := newTracerProvider(ctx)
	if err != nil {
		handleErr(err)
		return shutdown, err
	}
	shutdownFuncs = append(shutdownFuncs, tracerProvider.Shutdown)
	otel.SetTracerProvider(tracerProvider)

	// Set up meter provider.
	meterProvider, err := newMeterProvider()
	if err != nil {
		handleErr(err)
		return shutdown, err
	}
	shutdownFuncs = append(shutdownFuncs, meterProvider.Shutdown)
	otel.SetMeterProvider(meterProvider)

	// Set up logger provider.
	loggerProvider, err := newLoggerProvider()
	if err != nil {
		handleErr(err)
		return shutdown, err
	}
	shutdownFuncs = append(shutdownFuncs, loggerProvider.Shutdown)
	global.SetLoggerProvider(loggerProvider)

	return shutdown, err
}

func newTracerProvider(ctx context.Context) (*sdktrace.TracerProvider, error) {
	exporter, err := otlptracegrpc.New(
		ctx,
		otlptracegrpc.WithInsecure(),
		otlptracegrpc.WithEndpoint("localhost:4317"),
	)
	if err != nil {
		return nil, err
	}

	res, err := resource.New(
		ctx,
		resource.WithAttributes(
			semconv.ServiceName("assyl-backend"),
		),
	)
	if err != nil {
		return nil, err
	}

	tracerProvider := sdktrace.NewTracerProvider(
		sdktrace.WithBatcher(
			exporter,
			sdktrace.WithBatchTimeout(time.Second),
		),
		sdktrace.WithResource(res),
	)
	return tracerProvider, nil
}

func newPropagator() propagation.TextMapPropagator {
	return propagation.NewCompositeTextMapPropagator(
		propagation.TraceContext{},
		propagation.Baggage{},
	)
}

func newMeterProvider() (*metric.MeterProvider, error) {
	metricExporter, err := stdoutmetric.New()
	if err != nil {
		return nil, err
	}

	meterProvider := metric.NewMeterProvider(
		metric.WithReader(metric.NewPeriodicReader(metricExporter,
			// Default is 1m. Set to 3s for demonstrative purposes.
			metric.WithInterval(3*time.Second))),
	)
	return meterProvider, nil
}

func newLoggerProvider() (*log.LoggerProvider, error) {
	logExporter, err := stdoutlog.New()
	if err != nil {
		return nil, err
	}

	loggerProvider := log.NewLoggerProvider(
		log.WithProcessor(log.NewBatchProcessor(logExporter)),
	)
	return loggerProvider, nil
}
