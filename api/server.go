package api

import (
	"bytes"
	"context"
	"crypto/sha1"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	log "github.com/Sirupsen/logrus"
	"github.com/docker/docker/api/types"
	docker "github.com/docker/docker/client"
	"github.com/docker/docker/pkg/jsonmessage"
	"github.com/docker/docker/pkg/term"
	"github.com/ghodss/yaml"
	"github.com/gorilla/websocket"
	"github.com/julienschmidt/httprouter"
	"k8s.io/helm/pkg/chartutil"
	"k8s.io/helm/pkg/helm"
	"k8s.io/helm/pkg/proto/hapi/release"
	"k8s.io/helm/pkg/storage/driver"
	"k8s.io/helm/pkg/strvals"

	"github.com/deis/prow/pkg/version"
)

var WebsocketUpgrader = websocket.Upgrader{
	EnableCompression: true,
	// reduce the WriteBufferSize so `docker build` and `docker push` responses aren't internally
	// buffered by gorilla/websocket, but smaller, more informative messages can still be buffered.
	// https://github.com/gorilla/websocket/blob/9bc973af0682dc73a22553a08bfe00ee6255f56f/conn.go#L586-L593
	WriteBufferSize: 128,
}

// APIServer is an API Server which listens and responds to HTTP requests.
type APIServer struct {
	HTTPServer   *http.Server
	Listener     net.Listener
	DockerClient *docker.Client
	HelmClient   *helm.Client
	// RegistryAuth is the authorization token used to push images up to the registry.
	//
	// This field follows the format of the X-Registry-Auth header.
	RegistryAuth string
	// RegistryOrg is the organization (e.g. your DockerHub account) used to push images
	// up to the registry.
	RegistryOrg string
	// RegistryURL is the URL of the registry (e.g. quay.io, docker.io, gcr.io)
	RegistryURL string
	// Requests is a mapping of URLs that are currently being accessed by someone
	Requests     map[string]bool
	RequestsLock *sync.Mutex
}

// Serve starts the HTTP server, accepting all new connections.
func (s *APIServer) Serve() error {
	return s.HTTPServer.Serve(s.Listener)
}

// Close shuts down the HTTP server, dropping all current connections.
func (s *APIServer) Close() error {
	return s.Listener.Close()
}

// ServeRequest processes a single HTTP request.
func (s *APIServer) ServeRequest(w http.ResponseWriter, req *http.Request) {
	s.HTTPServer.Handler.ServeHTTP(w, req)
}

func (s *APIServer) createRouter() {
	r := httprouter.New()

	routerMap := map[string]map[string]httprouter.Handle{
		"GET": {
			"/ping":    ping,
			"/version": getVersion,
		},
		"POST": {
			"/apps/:id": s.BuildMiddleware(buildApp),
		},
	}

	for method, routes := range routerMap {
		for route, funct := range routes {
			// disable logging on /ping requests
			if route != "/ping" {
				funct = logRequestMiddleware(funct)
			}
			r.Handle(method, route, s.ServerMiddleware(funct))
		}
	}
	s.HTTPServer.Handler = r
}

func (s *APIServer) ServerMiddleware(h httprouter.Handle) httprouter.Handle {
	return func(w http.ResponseWriter, r *http.Request, p httprouter.Params) {
		// attach the API server to the request params so that it can retrieve info about itself
		ctx := context.WithValue(r.Context(), "server", s)
		// Delegate request to the given handle
		h(w, r.WithContext(ctx), p)
	}
}

func (s *APIServer) BuildMiddleware(h httprouter.Handle) httprouter.Handle {
	return func(w http.ResponseWriter, r *http.Request, p httprouter.Params) {
		s.RequestsLock.Lock()
		inUse := s.Requests[r.URL.Path]
		s.RequestsLock.Unlock()
		// if the request is currently in use, notify the user and tell them to try again later.
		if inUse {
			http.Error(
				w,
				fmt.Sprintf("Someone is currently using %s. Please try again later.", r.URL.Path),
				http.StatusConflict)
			return
		}
		s.RequestsLock.Lock()
		s.Requests[r.URL.Path] = true
		s.RequestsLock.Unlock()
		// Delegate request to the given handle
		h(w, r, p)
		// release the URL
		s.RequestsLock.Lock()
		s.Requests[r.URL.Path] = false
		s.RequestsLock.Unlock()
	}
}

// NewServer sets up the required Server and does protocol specific checking.
func NewServer(proto, addr string) (*APIServer, error) {
	var (
		a   *APIServer
		err error
	)
	switch proto {
	case "tcp":
		a, err = setupTCPHTTP(addr)
	case "unix":
		a, err = setupUnixHTTP(addr)
	default:
		a, err = nil, fmt.Errorf("Invalid protocol format.")
	}
	a.createRouter()
	a.Requests = make(map[string]bool)
	a.RequestsLock = &sync.Mutex{}
	return a, err
}

func setupTCPHTTP(addr string) (*APIServer, error) {
	l, err := net.Listen("tcp", addr)
	if err != nil {
		return nil, err
	}

	a := &APIServer{
		HTTPServer: &http.Server{Addr: addr},
		Listener:   l,
	}
	return a, nil
}

func setupUnixHTTP(addr string) (*APIServer, error) {
	if err := syscall.Unlink(addr); err != nil && !os.IsNotExist(err) {
		return nil, err
	}
	mask := syscall.Umask(0777)
	defer syscall.Umask(mask)

	l, err := net.Listen("unix", addr)
	if err != nil {
		return nil, err
	}

	if err := os.Chmod(addr, 0660); err != nil {
		return nil, err
	}

	a := &APIServer{
		HTTPServer: &http.Server{Addr: addr},
		Listener:   l,
	}
	return a, nil
}

func logRequestMiddleware(h httprouter.Handle) httprouter.Handle {
	return func(w http.ResponseWriter, r *http.Request, p httprouter.Params) {
		log.Infof("%s %s", r.Method, r.RequestURI)
		// Delegate request to the given handle
		h(w, r, p)
	}
}

// writeJSON writes the value v to the http response stream as json with standard
// json encoding.
func writeJSON(w http.ResponseWriter, v interface{}, code int) error {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	return json.NewEncoder(w).Encode(v)
}

func ping(w http.ResponseWriter, r *http.Request, p httprouter.Params) {
	w.Write([]byte{'P', 'O', 'N', 'G'})
}

func getVersion(w http.ResponseWriter, r *http.Request, p httprouter.Params) {
	if err := writeJSON(w, version.New(), http.StatusOK); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
	}
}

func buildApp(w http.ResponseWriter, r *http.Request, p httprouter.Params) {
	var imagePrefix string
	baseValues := map[string]interface{}{}
	appName := p.ByName("id")
	server := r.Context().Value("server").(*APIServer)
	namespace := r.Header.Get("Kubernetes-Namespace")
	flagWait := r.Header.Get("Helm-Flag-Wait")

	// load client values as the base config
	log.Debugf("Helm-Flag-Set: %s", r.Header.Get("Helm-Flag-Set"))

	userVals, err := base64.StdEncoding.DecodeString(r.Header.Get("Helm-Flag-Set"))
	if err != nil {
		http.Error(w, fmt.Sprintf("error while parsing header 'Helm-Flag-Set': %v\n", err), http.StatusBadRequest)
	}
	if err := yaml.Unmarshal([]byte(userVals), &baseValues); err != nil {
		http.Error(w, fmt.Sprintf("error while unmarshalling header 'Helm-Flag-Set' to yaml: %v\n", err), http.StatusBadRequest)
		return
	}

	// NOTE(bacongobbler): If no header was set, we default back to the default namespace.
	if namespace == "" {
		namespace = "default"
	}

	if r.Method != "POST" {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}

	optionWait, err := strconv.ParseBool(flagWait)
	if err != nil {
		http.Error(w, fmt.Sprintf("error while parsing header 'Helm-Flag-Wait': %v\n", err), http.StatusBadRequest)
		return
	}

	// this is just a buffer of 32MB. Everything is piped over to docker's build context and to
	// Helm so this is just a sane default in case docker or helm's backed up somehow.
	r.ParseMultipartForm(32 << 20)
	buildContext, _, err := r.FormFile("release-tar")
	if err != nil {
		http.Error(w, fmt.Sprintf("error while reading release-tar: %v\n", err), http.StatusBadRequest)
		return
	}
	defer buildContext.Close()

	chartFile, _, err := r.FormFile("chart-tar")
	if err != nil {
		http.Error(w, fmt.Sprintf("error while reading chart-tar: %v\n", err), http.StatusBadRequest)
		return
	}
	defer chartFile.Close()

	conn, err := WebsocketUpgrader.Upgrade(w, r, nil)
	if err != nil {
		http.Error(w, fmt.Sprintf("error when upgrading connection: %v\n", err), http.StatusInternalServerError)
		return
	}
	defer conn.Close()

	conn.SetCloseHandler(func(code int, text string) error {
		// Note https://tools.ietf.org/html/rfc6455#section-5.5 which specifies control
		// frames MUST be less than 125 bytes (This includes Close, Ping and Pong)
		// Hence, sending text as TextMessage and then sending control message.
		conn.WriteMessage(websocket.TextMessage, []byte(text))
		conn.WriteControl(websocket.CloseMessage, websocket.FormatCloseMessage(code, ""), time.Now().Add(time.Second))
		return nil
	})

	// write build context to a buffer so we can also write to the sha1 hash
	buf := new(bytes.Buffer)
	buildContextChecksum := sha1.New()
	mw := io.MultiWriter(buf, buildContextChecksum)
	io.Copy(mw, buildContext)

	// truncate checksum to the first 40 characters (20 bytes)
	// this is the equivalent of `shasum build.tar.gz | awk '{print $1}'`
	tag := fmt.Sprintf("%.20x", buildContextChecksum.Sum(nil))
	if server.RegistryOrg != "" {
		imagePrefix = server.RegistryOrg + "/"
	}
	imageName := fmt.Sprintf("%s/%s%s:%s",
		server.RegistryURL,
		imagePrefix,
		appName,
		tag,
	)

	// inject certain values into the chart such as the registry location, the application name
	// and the version
	imageVals := fmt.Sprintf("image.name=%s,image.org=%s,image.registry=%s,image.tag=%s",
		appName,
		server.RegistryOrg,
		server.RegistryURL,
		tag)

	if err := strvals.ParseInto(imageVals, baseValues); err != nil {
		handleClosingError(conn, "Could not inject registry data into values", err)
	}

	rawVals, err := yaml.Marshal(baseValues)
	if err != nil {
		handleClosingError(conn, "Could not marshal values", err)
	}

	// send uploaded tar to docker as the build context
	conn.WriteMessage(websocket.TextMessage, []byte("--> Building Dockerfile\n"))
	buildResp, err := server.DockerClient.ImageBuild(
		context.Background(),
		buf,
		types.ImageBuildOptions{
			Tags: []string{imageName},
		})
	if err != nil {
		handleClosingError(conn, "Could not build image from build context", err)
	}
	defer buildResp.Body.Close()
	writer, err := conn.NextWriter(websocket.TextMessage)
	if err != nil {
		handleClosingError(conn, "There was an error fetching a text message writer", err)
	}
	outFd, isTerm := term.GetFdInfo(writer)
	if err := jsonmessage.DisplayJSONMessagesStream(buildResp.Body, writer, outFd, isTerm, nil); err != nil {
		handleClosingError(conn, "Error encountered streaming JSON response", err)
	}

	_, _, err = server.DockerClient.ImageInspectWithRaw(
		context.Background(),
		imageName)
	if err != nil {
		if docker.IsErrImageNotFound(err) {
			handleClosingError(conn, fmt.Sprintf("Could not locate image for %s", appName), err)
		} else {
			handleClosingError(conn, "ImageInspectWithRaw error", err)
		}
	}

	conn.WriteMessage(websocket.TextMessage, []byte(fmt.Sprintf("--> Pushing %s\n", imageName)))
	pushResp, err := server.DockerClient.ImagePush(
		context.Background(),
		imageName,
		types.ImagePushOptions{RegistryAuth: server.RegistryAuth})
	if err != nil {
		handleClosingError(conn, fmt.Sprintf("Could not push %s to registry", imageName), err)
	}
	defer pushResp.Close()
	writer, err = conn.NextWriter(websocket.TextMessage)
	if err != nil {
		handleClosingError(conn, "There was an error fetching a text message writer", err)
	}
	outFd, isTerm = term.GetFdInfo(writer)
	if err := jsonmessage.DisplayJSONMessagesStream(pushResp, writer, outFd, isTerm, nil); err != nil {
		handleClosingError(conn, "Error encountered streaming JSON response", err)
	}

	conn.WriteMessage(websocket.TextMessage, []byte("--> Deploying to Kubernetes\n"))
	chart, err := chartutil.LoadArchive(chartFile)
	if err != nil {
		handleClosingError(conn, "Could not load chart archive", err)
	}
	// If a release does not exist, install it. If another error occurs during
	// the check, ignore the error and continue with the upgrade.
	//
	// The returned error is a grpc.rpcError that wraps the message from the original error.
	// So we're stuck doing string matching against the wrapped error, which is nested somewhere
	// inside of the grpc.rpcError message.
	_, err = server.HelmClient.ReleaseContent(appName, helm.ContentReleaseVersion(1))
	if err != nil && strings.Contains(err.Error(), driver.ErrReleaseNotFound.Error()) {
		conn.WriteMessage(
			websocket.TextMessage,
			[]byte(fmt.Sprintf("    Release %q does not exist. Installing it now.\n", appName)))
		releaseResp, err := server.HelmClient.InstallReleaseFromChart(
			chart,
			namespace,
			helm.ReleaseName(appName),
			helm.ValueOverrides([]byte(rawVals)),
			helm.InstallWait(optionWait))
		if err != nil {
			handleClosingError(conn, "Could not install release", err)
		}
		conn.WriteMessage(
			websocket.TextMessage,
			formatReleaseStatus(releaseResp.Release))
	} else {
		releaseResp, err := server.HelmClient.UpdateReleaseFromChart(
			appName,
			chart,
			helm.UpdateValueOverrides([]byte(rawVals)),
			helm.UpgradeWait(optionWait))
		if err != nil {
			handleClosingError(conn, "Could not upgrade release", err)
		}
		conn.WriteMessage(
			websocket.TextMessage,
			formatReleaseStatus(releaseResp.Release))
	}

	// gently tell the client that we are closing the connection
	conn.WriteControl(
		websocket.CloseMessage,
		websocket.FormatCloseMessage(websocket.CloseNormalClosure, ""),
		time.Now().Add(time.Second))
}

// handleClosingError formats the err and corresponding verbiage and invokes
// conn.CloseHandler() as set by conn.SetCloseHandler()
func handleClosingError(conn *websocket.Conn, verbiage string, err error) {
	conn.CloseHandler()(
		websocket.CloseInternalServerErr,
		fmt.Sprintf("%s: %v\n", verbiage, err))
}

// formatReleaseStatus returns a byte slice of formatted release status information
func formatReleaseStatus(release *release.Release) []byte {
	output := fmt.Sprintf("--> Status: %s\n", release.Info.Status.Code.String())
	if release.Info.Status.Notes != "" {
		output += fmt.Sprintf("--> Notes:\n     %s\n", release.Info.Status.Notes)
	}
	return []byte(output)
}
