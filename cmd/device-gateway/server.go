package main

import (
	"encoding/json"
	"fmt"
	"io/ioutil"
	"log"
	"mime"
	"net"
	"net/http"
	"path/filepath"
	"strings"
	"time"

	catalog "github.com/patchwork-toolkit/patchwork/catalog/device"
)

// errorResponse used to serialize errors into JSON for RESTful responses
type errorResponse struct {
	Error string `json:"error"`
}

// RESTfulAPI contains all required configuration for running a RESTful API
// for device gateway
type RESTfulAPI struct {
	config     *Config
	restConfig *RestProtocol
	serverMux  *http.ServeMux
	dataCh     chan<- DataRequest
}

// Constructs a RESTfulAPI data structure
func newRESTfulAPI(conf *Config, dataCh chan<- DataRequest) *RESTfulAPI {
	restConfig, _ := conf.Protocols[ProtocolTypeREST].(RestProtocol)

	api := &RESTfulAPI{
		config:     conf,
		restConfig: &restConfig,
		serverMux:  http.NewServeMux(),
		dataCh:     dataCh,
	}
	return api
}

// Setup all routers, handlers and start a HTTP server (blocking call)
func (self *RESTfulAPI) start(catalogStorage catalog.CatalogStorage) {
	self.mountCatalog(catalogStorage)
	self.mountResources()
	self.serverMux.Handle("/dashboard", self.dashboardHandler(*confPath))
	self.serverMux.Handle(self.restConfig.Location, self.indexHandler())
	self.serverMux.Handle(StaticLocation, self.staticHandler())

	s := &http.Server{
		Handler:        self.serverMux,
		ReadTimeout:    30 * time.Second,
		WriteTimeout:   30 * time.Second,
		MaxHeaderBytes: 1 << 20,
	}

	addr := fmt.Sprintf("%v:%v", self.config.Http.BindAddr, self.config.Http.BindPort)
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		log.Println(err.Error())
		return
	}

	log.Printf("Starting server at http://%v%v", addr, self.restConfig.Location)

	err = s.Serve(ln)
	if err != nil {
		log.Println(err.Error())
	}
}

// Create a HTTP handler to serve and update dashboard configuration
func (self *RESTfulAPI) dashboardHandler(confPath string) http.HandlerFunc {
	dashboardConfPath := filepath.Join(filepath.Dir(confPath), "dashboard.json")

	return func(rw http.ResponseWriter, req *http.Request) {
		rw.Header().Set("Content-Type", "application/json")

		if req.Method == "POST" {
			body, err := ioutil.ReadAll(req.Body)
			req.Body.Close()
			if err != nil {
				self.respondWithBadRequest(rw, err.Error())
				return
			}

			err = ioutil.WriteFile(dashboardConfPath, body, 0755)
			if err != nil {
				rw.WriteHeader(http.StatusInternalServerError)
				errData := map[string]string{"error": err.Error()}
				b, _ := json.Marshal(errData)
				rw.Write(b)
				return
			}

			rw.WriteHeader(http.StatusCreated)
			rw.Write([]byte("{}"))

		} else if req.Method == "GET" {
			data, err := ioutil.ReadFile(dashboardConfPath)
			if err != nil {
				rw.WriteHeader(http.StatusInternalServerError)
				errData := map[string]string{"error": err.Error()}
				b, _ := json.Marshal(errData)
				rw.Write(b)
				return
			}
			rw.WriteHeader(http.StatusOK)
			rw.Write(data)
		} else {
			rw.WriteHeader(http.StatusMethodNotAllowed)
		}
	}
}

func (self *RESTfulAPI) indexHandler() http.HandlerFunc {
	return func(rw http.ResponseWriter, req *http.Request) {
		b, _ := json.Marshal("Welcome to Device Gateway RESTful API")
		rw.Header().Set("Content-Type", "application/json")
		rw.Write(b)
	}
}

func (self *RESTfulAPI) staticHandler() http.HandlerFunc {
	return func(rw http.ResponseWriter, req *http.Request) {

		// serve all /static/ctx files as ld+json
		if strings.HasPrefix(req.URL.Path, "/static/ctx") {
			rw.Header().Set("Content-Type", "application/ld+json")
		}
		filePath := strings.Join(strings.Split(req.URL.Path, "/")[2:], "/")
		http.ServeFile(rw, req, self.config.StaticDir+"/"+filePath)
	}
}

func (self *RESTfulAPI) mountResources() {
	for _, device := range self.config.Devices {
		for _, resource := range device.Resources {
			for _, protocol := range resource.Protocols {
				if protocol.Type != ProtocolTypeREST {
					continue
				}
				uri := self.restConfig.Location + "/" + device.Name + "/" + resource.Name
				log.Println("RESTfulAPI: Mounting resource:", uri)
				rid := device.ResourceId(resource.Name)
				for _, method := range protocol.Methods {
					switch method {
					case "GET":
						self.serverMux.Handle(uri, self.createResourceGetHandler(rid))
					case "PUT":
						self.serverMux.Handle(uri, self.createResourcePutHandler(rid))
					}
				}
			}
		}
	}
}

func (self *RESTfulAPI) mountCatalog(catalogStorage catalog.CatalogStorage) {
	catalogAPI := catalog.NewReadableCatalogAPI(catalogStorage, CatalogLocation, StaticLocation,
		fmt.Sprintf("Local catalog at %s", self.config.Description))

	self.serverMux.Handle(fmt.Sprintf("%s/%s/%s/%s/%s",
		CatalogLocation, catalog.PatternFType, catalog.PatternFPath, catalog.PatternFOp, catalog.PatternFValue),
		http.HandlerFunc(catalogAPI.Filter))

	self.serverMux.Handle(fmt.Sprintf("%s/%s/%s/%s",
		CatalogLocation, catalog.PatternUuid, catalog.PatternReg, catalog.PatternRes),
		http.HandlerFunc(catalogAPI.GetResource))

	self.serverMux.Handle(fmt.Sprintf("%s/%s/%s",
		CatalogLocation, catalog.PatternUuid, catalog.PatternReg),
		http.HandlerFunc(catalogAPI.Get))

	self.serverMux.Handle(CatalogLocation, http.HandlerFunc(catalogAPI.List))

	log.Printf("Mounted local catalog at %v", CatalogLocation)
}

func (self *RESTfulAPI) createResourceGetHandler(resourceId string) http.HandlerFunc {
	return func(rw http.ResponseWriter, req *http.Request) {
		log.Printf("RESTfulAPI: %s %s", req.Method, req.RequestURI)

		// Resolve mediaType
		v := req.Header.Get("Content-Type")
		mediaType, _, err := mime.ParseMediaType(v)
		if err != nil {
			self.respondWithBadRequest(rw, err.Error())
			return
		}

		// Check if mediaType is supported by resource
		isSupported := false
		resource, found := self.config.FindResource(resourceId)
		if !found {
			self.respondWithNotFound(rw, "Resource does not exist")
			return
		}
		for _, p := range resource.Protocols {
			if p.Type == ProtocolTypeREST {
				isSupported = true
			}
		}
		if !isSupported {
			self.respondWithUnsupportedMediaType(rw, "Media type is not supported by this resource")
			return
		}

		// Retrieve data
		dr := DataRequest{
			ResourceId: resourceId,
			Type:       DataRequestTypeRead,
			Arguments:  nil,
			Reply:      make(chan AgentResponse),
		}
		self.dataCh <- dr

		// Wait for the response
		repl := <-dr.Reply

		// Response to client
		rw.Header().Set("Content-Type", mediaType)
		if repl.IsError {
			self.respondWithInternalServerError(rw, string(repl.Payload))
			return
		}
		rw.Write(repl.Payload)
	}
}

func (self *RESTfulAPI) createResourcePutHandler(resourceId string) http.HandlerFunc {
	return func(rw http.ResponseWriter, req *http.Request) {
		log.Printf("RESTfulAPI: %s %s", req.Method, req.RequestURI)

		// Resolve mediaType
		v := req.Header.Get("Content-Type")
		mediaType, _, err := mime.ParseMediaType(v)
		if err != nil {
			self.respondWithBadRequest(rw, err.Error())
			return
		}

		// Check if mediaType is supported by resource
		isSupported := false
		resource, found := self.config.FindResource(resourceId)
		if !found {
			self.respondWithNotFound(rw, "Resource does not exist")
			return
		}
		for _, p := range resource.Protocols {
			if p.Type == ProtocolTypeREST {
				isSupported = true
			}
		}
		if !isSupported {
			self.respondWithUnsupportedMediaType(rw, "Media type is not supported by this resource")
			return
		}

		// Extract PUT's body
		body, err := ioutil.ReadAll(req.Body)
		req.Body.Close()
		if err != nil {
			self.respondWithBadRequest(rw, err.Error())
			return
		}

		// Submit data request
		dr := DataRequest{
			ResourceId: resourceId,
			Type:       DataRequestTypeWrite,
			Arguments:  body,
			Reply:      make(chan AgentResponse),
		}
		log.Printf("RESTfulAPI: Submitting data request %#v", dr)
		self.dataCh <- dr

		// Wait for the response
		repl := <-dr.Reply

		// Respond to client
		rw.Header().Set("Content-Type", mediaType)
		if repl.IsError {
			self.respondWithInternalServerError(rw, string(repl.Payload))
			return
		}
		rw.WriteHeader(http.StatusNoContent)
	}
}

func (self *RESTfulAPI) respondWithNotFound(rw http.ResponseWriter, msg string) {
	rw.Header().Set("Content-Type", "application/json")
	rw.WriteHeader(http.StatusNotFound)
	err := &errorResponse{Error: msg}
	b, _ := json.Marshal(err)
	rw.Write(b)
}

func (self *RESTfulAPI) respondWithBadRequest(rw http.ResponseWriter, msg string) {
	rw.Header().Set("Content-Type", "application/json")
	rw.WriteHeader(http.StatusBadRequest)
	err := &errorResponse{Error: msg}
	b, _ := json.Marshal(err)
	rw.Write(b)
}

func (self *RESTfulAPI) respondWithUnsupportedMediaType(rw http.ResponseWriter, msg string) {
	rw.Header().Set("Content-Type", "application/json")
	rw.WriteHeader(http.StatusUnsupportedMediaType)
	err := &errorResponse{Error: msg}
	b, _ := json.Marshal(err)
	rw.Write(b)
}

func (self *RESTfulAPI) respondWithInternalServerError(rw http.ResponseWriter, msg string) {
	rw.Header().Set("Content-Type", "application/json")
	rw.WriteHeader(http.StatusInternalServerError)
	err := &errorResponse{Error: msg}
	b, _ := json.Marshal(err)
	rw.Write(b)
}
