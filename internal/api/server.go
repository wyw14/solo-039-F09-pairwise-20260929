package api

import (
	"github.com/go-chi/chi/v5"
	"github.com/wyw14/solo-039/internal/service"
	"net/http"
)

type Server struct {
	runtime *service.Runtime
	plant   *service.Plant
	router  chi.Router
}

func NewServer() *Server {
	plant := service.NewPlant("data")
	_ = plant.Restore()
	return NewServerWithPlant(plant)
}

func NewServerWithPlant(plant *service.Plant) *Server {
	s := &Server{runtime: service.NewRuntime("baseline"), plant: plant, router: chi.NewRouter()}
	s.routes()
	return s
}

func (s *Server) Handler() http.Handler { return s.router }
