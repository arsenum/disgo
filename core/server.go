package core

import (
	"encoding/json"
	"io/ioutil"
	"os"
	"strings"
	"sync"
	"net"
	"github.com/dispatchlabs/disgo/properties"
	"github.com/dispatchlabs/disgo_commons/types"
	log "github.com/sirupsen/logrus"
	dapos "github.com/dispatchlabs/dapos/core"
	disgover "github.com/dispatchlabs/disgover/core"
	"strconv"
	"google.golang.org/grpc/reflection"
	"google.golang.org/grpc"
)

// Server
type Server struct {
	services []types.IService
}

// NewServer
func NewServer() *Server {

	// Setup log.
	formatter := &log.TextFormatter{
		FullTimestamp: true,
		ForceColors:   false,
	}
	log.SetFormatter(formatter)
	log.SetOutput(os.Stdout)
	log.SetLevel(log.InfoLevel)

	// Read configuration JSON file.
	filePath := "." + string(os.PathSeparator) + "properties" + string(os.PathSeparator) + "disgo.json"
	file, error := ioutil.ReadFile(filePath)
	if error != nil {
		log.Error("unable to load " + filePath)
		os.Exit(1)
	}
	json.Unmarshal(file, &properties.Properties)

	// Load Keys
	if _, _, err := loadKeys(); err != nil {
		log.Error("unable to keys: " + err.Error())
	}

	return &Server{}
}

// Start
func (server *Server) Start() {
	log.Info("args  [" + strings.Join(os.Args, " ") + "]")

	// Add services.
	server.services = append(server.services, dapos.NewDAPoSService())
	server.services = append(server.services, disgover.NewDisGoverService())

	// Create TCP listener/GRPC server.
	listener, error := net.Listen("tcp", ":"+strconv.Itoa(properties.Properties.GrpcPort))
	if error != nil {
		log.Fatalf("failed to listen: %v", error)
	}
	grpcServer := grpc.NewServer()

	// Initialize and run services.
	var waitGroup sync.WaitGroup
	for _, service := range server.services {
		log.Info("starting " + service.Name() + "...")
		service.Init()
		service.RegisterGrpc(grpcServer)
		go service.Go(&waitGroup)
	}

	// Serve.
	reflection.Register(grpcServer)
	log.WithFields(log.Fields{
		"method": "Server.Start",
	}).Info("listening on " + strconv.Itoa(properties.Properties.GrpcPort))
	if error := grpcServer.Serve(listener); error != nil {
		log.Fatalf("failed to serve: %v", error)
	}

	for i := 0; i < len(server.services); i++ {
		waitGroup.Add(1)
	}
	waitGroup.Wait()
}
