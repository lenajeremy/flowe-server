package api

import (
	"fmt"
	"log/slog"

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
	if err := s.routerEngine.Run(addr); err != nil {
		slog.Error("server stopped", "error", err)
	}
}
