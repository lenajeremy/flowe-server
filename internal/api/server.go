package api

import (
	"fmt"
	"log/slog"
	"net/http"
	"time"

	"workflow-ai/server/internal/database"

	"github.com/gin-gonic/gin"
	"github.com/redis/go-redis/v9"
)

type Server struct {
	port         int
	routerEngine *gin.Engine
	db           *database.DBClient
	redis        *redis.Client
}

func NewServer(port int, db *database.DBClient, rdb *redis.Client) *Server {
	return &Server{
		port:         port,
		routerEngine: gin.New(),
		db:           db,
		redis:        rdb,
	}
}

func (s *Server) Start(port int) {
	addr := fmt.Sprintf(":%d", port)
	slog.Info("server starting", "addr", addr)
	// Explicit timeouts harden against Slowloris and idle-connection exhaustion.
	// No WriteTimeout: responses stream via SSE (runs, AI chat) for minutes and
	// a write deadline would cut them off. MaxHeaderBytes caps header size;
	// request-body size is capped by BodyLimit middleware.
	srv := &http.Server{
		Addr:              addr,
		Handler:           s.routerEngine,
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       120 * time.Second,
		MaxHeaderBytes:    1 << 20, // 1 MiB
	}
	if err := srv.ListenAndServe(); err != nil {
		slog.Error("server stopped", "error", err)
	}
}
