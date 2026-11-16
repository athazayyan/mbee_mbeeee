package main

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/joho/godotenv"
	apidocs "kitakarsa.com/tenantku/backend/api"
	"kitakarsa.com/tenantku/backend/internal/auth"
	"kitakarsa.com/tenantku/backend/internal/booking"
	"kitakarsa.com/tenantku/backend/internal/events"
	"kitakarsa.com/tenantku/backend/internal/identity"
	"kitakarsa.com/tenantku/backend/internal/platform/httpapi"
)

func main() {
	if err := godotenv.Load(); err != nil && !errors.Is(err, os.ErrNotExist) {
		log.Fatal("could not load .env")
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if len(os.Args) == 2 && os.Args[1] == "migrate" {
		if err := migrate(ctx); err != nil {
			log.Fatal(err)
		}
		return
	}

	if err := run(ctx); err != nil {
		log.Fatal(err)
	}
}

func run(ctx context.Context) error {
	port := os.Getenv("PORT")
	if port == "" {
		port = "8080"
	}

	router := gin.Default()
	if os.Getenv("API_DOCS_ENABLED") == "true" {
		apidocs.Routes(router)
	}
	if err := router.SetTrustedProxies(nil); err != nil {
		return err
	}

	router.GET("/healthz", func(c *gin.Context) {
		c.JSON(http.StatusOK, gin.H{"status": "ok"})
	})

	api := router.Group("/api/v1", httpapi.RequestID, func(c *gin.Context) {
		c.Header("Cache-Control", "no-store")
	})
	if databaseURL := os.Getenv("DATABASE_URL"); databaseURL != "" {
		origin, err := appOrigin(os.Getenv("APP_ORIGIN"))
		if err != nil {
			return err
		}
		startup, cancel := context.WithTimeout(ctx, 10*time.Second)
		pool, err := pgxpool.New(startup, databaseURL)
		if err != nil {
			cancel()
			return errors.New("invalid PostgreSQL configuration")
		}
		defer pool.Close()
		service, err := auth.New(startup, pool, os.Getenv("JWT_SIGNING_KEY"), os.Getenv("REFRESH_TOKEN_PEPPER"))
		if err != nil {
			cancel()
			return err
		}
		profiles := identity.Store{DB: pool}
		eventStore := events.Store{DB: pool}
		bookings := booking.Store{DB: pool}
		checkSchema := func(probe context.Context) error {
			if err := profiles.Ready(probe); err != nil {
				return fmt.Errorf("identity schema unavailable; run make migrate: %w", err)
			}
			if err := eventStore.Ready(probe); err != nil {
				return fmt.Errorf("event schema unavailable; run make migrate: %w", err)
			}
			if err := bookings.Ready(probe); err != nil {
				return fmt.Errorf("booking schema unavailable; run make migrate: %w", err)
			}
			return nil
		}
		err = checkSchema(startup)
		cancel()
		if err != nil {
			return err
		}
		service.Routes(api, origin)
		profiles.Routes(api, service, origin)
		eventStore.Routes(api, service, origin)
		bookings.Routes(api, service)
		router.GET("/readyz", func(c *gin.Context) {
			probe, cancel := context.WithTimeout(c.Request.Context(), 2*time.Second)
			defer cancel()
			if service.Ready(probe) != nil || checkSchema(probe) != nil {
				c.JSON(http.StatusServiceUnavailable, gin.H{"status": "unavailable"})
				return
			}
			c.JSON(http.StatusOK, gin.H{"status": "ready"})
		})
	} else {
		router.GET("/readyz", func(c *gin.Context) {
			c.JSON(http.StatusServiceUnavailable, gin.H{"status": "unavailable"})
		})
		unavailable := func(c *gin.Context) {
			httpapi.Error(c, http.StatusServiceUnavailable, "AUTH_UNAVAILABLE", "Layanan akun belum tersedia.", nil)
		}
		api.POST("/auth/register", unavailable)
		api.POST("/auth/login", unavailable)
		api.POST("/auth/refresh", unavailable)
		api.POST("/auth/logout", unavailable)
		identity.Store{}.Routes(api, nil, "")
		events.Store{}.Routes(api, nil, "")
		booking.Store{}.Routes(api, nil)
	}

	server := &http.Server{
		Addr:              os.Getenv("HOST") + ":" + port,
		Handler:           router,
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       15 * time.Second,
		WriteTimeout:      15 * time.Second,
		IdleTimeout:       60 * time.Second,
	}

	errCh := make(chan error, 1)
	go func() { errCh <- server.ListenAndServe() }()
	select {
	case err := <-errCh:
		if !errors.Is(err, http.ErrServerClosed) {
			return err
		}
		return nil
	case <-ctx.Done():
		shutdown, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		return server.Shutdown(shutdown)
	}
}
