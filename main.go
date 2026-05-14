package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net/url"
	"os"
	"os/signal"
	"slices"
	"syscall"
	"time"

	"boot.dev/linko/internal/build"
	"boot.dev/linko/internal/linkoerr"
	"boot.dev/linko/internal/store"
	"github.com/lmittmann/tint"
	"github.com/mattn/go-isatty"
	pkgerr "github.com/pkg/errors"
	"gopkg.in/natefinch/lumberjack.v2"
)

func main() {
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)

	httpPort := flag.Int("port", 8899, "port to listen on")
	dataDir := flag.String("data", "./data", "directory to store data")
	flag.Parse()

	status := run(ctx, cancel, *httpPort, *dataDir)
	cancel()
	os.Exit(status)
}

func run(ctx context.Context, cancel context.CancelFunc, httpPort int, dataDir string) int {
	logger, closeFunc, err := initializeLogger(os.Getenv("LINKO_LOG_FILE"))
	if err != nil {
		slog.Error(fmt.Sprintf("failed to initialize logger: %v", err))
		os.Exit(1)
	}
	defer func() {
		if err := closeFunc(); err != nil {
			logger.Error(fmt.Sprintf("failed to close logger: %v", err))
		}
	}()

	env := os.Getenv("ENV")
	hostname, _ := os.Hostname()

	logger = logger.With(
		slog.String("git_sha", build.GitSHA),
		slog.String("build_time", build.BuildTime),
		slog.String("env", env),
		slog.String("hostname", hostname),
	)

	st, err := store.New(dataDir, logger)
	if err != nil {
		logger.Error(fmt.Sprintf("failed to create store: %v", err))
		return 1
	}

	s := newServer(*st, httpPort, cancel, logger)
	var serverErr error
	go func() {
		serverErr = s.start()
	}()

	logger.Debug(fmt.Sprintf("Linko is running on http://localhost:%d", httpPort))

	<-ctx.Done()

	logger.Debug("Linko is shutting down")

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	if err := s.shutdown(shutdownCtx); err != nil {
		logger.Error(fmt.Sprintf("failed to shutdown server: %v", err))
		return 1
	}
	if serverErr != nil {
		logger.Error(fmt.Sprintf("server error: %v", serverErr))
		return 1
	}
	return 0
}

type closeFunc func() error

func initializeLogger(logFile string) (*slog.Logger, closeFunc, error) {
	stderrHandler := tint.NewHandler(os.Stderr, &tint.Options{
		Level:       slog.LevelDebug,
		ReplaceAttr: replaceAttr,
		NoColor:     !(isatty.IsCygwinTerminal(os.Stderr.Fd()) || isatty.IsTerminal(os.Stderr.Fd())),
	})
	if logFile != "" {
		logger := &lumberjack.Logger{
			Filename:   logFile,
			MaxSize:    1,
			MaxAge:     28,
			MaxBackups: 10,
			LocalTime:  false,
			Compress:   true,
		}
		fileHandler := slog.NewJSONHandler(logger, &slog.HandlerOptions{
			Level:       slog.LevelInfo,
			ReplaceAttr: replaceAttr,
		})
		return slog.New(slog.NewMultiHandler(
				fileHandler,
				stderrHandler,
			)), func() error {
				return logger.Close()
			}, nil
	}
	return slog.New(stderrHandler), func() error {
		return nil
	}, nil
}

type multiError interface {
	error
	Unwrap() []error
}

type stackTracer interface {
	error
	StackTrace() pkgerr.StackTrace
}

var sensitiveKeys = []string{"user", "password", "key", "apikey", "secret", "pin", "creditcardno"}

func safeDSN(dsn string) string {
	parsed, err := url.Parse(dsn)
	if err != nil {
		return "invalid dsn"
	}
	_, ok := parsed.User.Password()
	if !ok {
		return "no credentials"
	}
	parsed.User = url.UserPassword(parsed.User.Username(), "[REDACTED]")
	return parsed.String()
}

func replaceAttr(groups []string, a slog.Attr) slog.Attr {
	if slices.Contains(sensitiveKeys, a.Key) {
		return slog.String(a.Key, "[REDACTED]")
	}
	if a.Key == "error" {
		err, ok := a.Value.Any().(error)
		if !ok {
			return a
		}

		if multiErr, ok := errors.AsType[multiError](err); ok {
			errs := multiErr.Unwrap()
			errAttrs := make([]slog.Attr, 0, len(errs))
			for i, e := range errs {
				errAttrs = append(errAttrs, slog.Attr{
					Key: fmt.Sprintf("error_%d", i+1),
					Value: replaceAttr(groups, slog.Attr{
						Key:   "error",
						Value: slog.AnyValue(e),
					}).Value})
			}
			return slog.GroupAttrs("errors", errAttrs...)
		}

		attrs := linkoerr.Attrs(err)

		if stackErr, ok := errors.AsType[stackTracer](err); ok {
			attrs = append(attrs, slog.Attr{
				Key:   "stack_trace",
				Value: slog.StringValue(fmt.Sprintf("%+v", stackErr.StackTrace())),
			})
		}

		if len(attrs) > 0 {
			groupAttrs := []slog.Attr{
				slog.String("message", err.Error()),
			}
			groupAttrs = append(groupAttrs, attrs...)
			return slog.GroupAttrs("error", groupAttrs...)
		}

		return slog.String("error", fmt.Sprintf("%+v", err))
	}
	if a.Value.Kind() == slog.KindString {
		safeDSN := safeDSN(a.Value.String())
		if safeDSN != "invalid dsn" && safeDSN != "no credentials" {
			return slog.String(a.Key, safeDSN)
		}
	}
	return a
}
