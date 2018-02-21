package core

import (
	"encoding/json"
	"io/ioutil"
	"os"
	"strings"
	"sync"
	"github.com/dispatchlabs/disgo/properties"
	"github.com/dispatchlabs/disgo_commons/types"
	log "github.com/sirupsen/logrus"
	dapos "github.com/dispatchlabs/dapos/core"
	disgover "github.com/dispatchlabs/disgover/core"
	"github.com/dispatchlabs/disgo_commons/services"
	"reflect"
)

const (
	Version = "1.0.0"
)

// Server
type Server struct {
	services   []types.IService
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

// Go
func (server *Server) Go() {
	log.Info("booting Disgo v" + Version + "...")
	log.Info("args  [" + strings.Join(os.Args, " ") + "]")

	// Add services.
	server.services = append(server.services, dapos.NewDAPoSService())
	server.services = append(server.services, disgover.NewDisGoverService())
	server.services = append(server.services, services.NewHttpService())
	server.services = append(server.services, services.NewGrpcService())

	// Run services.
	var waitGroup sync.WaitGroup
	for _, service := range server.services {
		log.WithFields(log.Fields{
			"method": "Server.Go",
		}).Info("starting " + service.Name() + "...")
		go service.Go(&waitGroup)
		waitGroup.Add(1)
	}
	waitGroup.Wait()
}

/*
func (server *Server) createTransactionHandler(responseWriter http.ResponseWriter, request *http.Request) {
	body, error := ioutil.ReadAll(request.Body)
	if error != nil {
		log.WithFields(log.Fields{
			"method": "Server.createTransactionHandler",
		}).Error("unable to read HTTP body of request ", error)
		http.Error(responseWriter, "error reading HTTP body of request", http.StatusBadRequest)
		return
	}

	transaction, error := types.NewTransactionFromJson(body)
	if error != nil {
		log.WithFields(log.Fields{
			"method": "Server.createTransactionHandler",
		}).Error("JSON_PARSE_ERROR ", error) // TODO: Should return JSON!!!
		http.Error(responseWriter, "error reading HTTP body of request", http.StatusBadRequest)
		return
	}

	transaction, error = server.getService(&dapos.DAPoSService{}).(*dapos.DAPoSService).CreateTransaction(transaction, nil)
	if error != nil {
		log.WithFields(log.Fields{
			"method": "Server.createTransactionHandler",
		}).Error("JSON_PARSE_ERROR ", error) // TODO: Should return JSON!!!
		http.Error(responseWriter, "error reading HTTP body of request", http.StatusBadRequest)
		return
	}

	http.Error(responseWriter, "foobar", http.StatusOK)
}
*/

// getService
func (server *Server) getService(serviceInterface interface{}) types.IService {
	for _, service := range server.services {
		if reflect.TypeOf(service) == reflect.TypeOf(serviceInterface) {
			return service
		}
	}
	return nil
}
