package main

import (
	"bytes"
	"context"
	"flag"
	"fmt"
	"html/template"
	"io"
	"log"
	"mime"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"path"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"github.com/evanj/googlesignin/iap"
	"golang.org/x/net/html"
	"golang.org/x/net/html/atom"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
)

const portEnvVar = "PORT"
const defaultPort = "8080"
const htmlMediaType = "text/html"
const googleHealthCheckUserAgent = "googlehc/"
const kubernetesHealthCheckUserAgent = "kube-probe/"

var servicePattern = regexp.MustCompile(`^/([^/]+)/([^/]+)/([^/]+)(.*)$`)

type serviceInfo interface {
	list(ctx context.Context, limit int64) (*corev1.ServiceList, error)
	get(ctx context.Context, namespace string, name string) (*corev1.Service, error)
}

type kubernetesAPIClient struct {
	clientset *kubernetes.Clientset
}

func (k *kubernetesAPIClient) list(ctx context.Context, limit int64) (*corev1.ServiceList, error) {
	return k.clientset.CoreV1().Services("").List(ctx, metav1.ListOptions{Limit: limit})
}
func (k *kubernetesAPIClient) get(ctx context.Context, namespace string, name string) (*corev1.Service, error) {
	return k.clientset.CoreV1().Services(namespace).Get(ctx, name, metav1.GetOptions{})
}

type origRequestData struct {
	namespace string
	service   string
	port      int64
	destPath  string
}

type origRequestDataContextKey struct{}

type server struct {
	services     serviceInfo
	reverseProxy *httputil.ReverseProxy
}

func newServer(services serviceInfo) *server {
	s := &server{services: services}
	s.reverseProxy = &httputil.ReverseProxy{
		// Director does nothing: we rewrite in proxy
		Director:       func(*http.Request) {},
		ModifyResponse: s.proxyRewriter,
	}
	return s
}

func (s *server) checkPermissions(ctx context.Context) error {
	// attempt to list a single service to see if we have permission
	_, err := s.services.list(ctx, 1)
	return err
}

func (s *server) healthHandler(w http.ResponseWriter, r *http.Request) {
	log.Printf("health check %s URL=%s RemoteAddr=%s UserAgent=%s",
		r.Method, r.URL.String(), r.RemoteAddr, r.UserAgent())
	if r.Method != http.MethodGet {
		http.Error(w, "invalid method", http.StatusMethodNotAllowed)
		return
	}

	w.Header().Set("Content-Type", "text/plain;charset=utf-8")
	w.Write([]byte("ok\n"))
}

var healthCheckUserAgents = []string{
	googleHealthCheckUserAgent, kubernetesHealthCheckUserAgent,
}

// Returns true if this looks like a health check on /. GKE custom path health checks are very
// fragile: its easier to support health checks on / for some user agents. See:
// https://github.com/kubernetes/ingress-gce/issues/42
func isRootHealthCheck(r *http.Request) bool {
	if r.URL.Path != "/" {
		return false
	}

	lowerUserAgent := strings.ToLower(r.UserAgent())
	for _, healthCheckAgent := range healthCheckUserAgents {
		if strings.Contains(lowerUserAgent, healthCheckAgent) {
			return true
		}
	}
	return false
}

func (s *server) rootHandler(w http.ResponseWriter, r *http.Request) {
	if servicePattern.MatchString(r.URL.Path) {
		s.proxyErrWrapper(w, r)
		return
	}

	// TODO: Add back email/user log once we add the test library to iap
	// email := iap.Email(r)
	// log.Printf("rootHandler user=%s %s %s", email, r.Method, r.URL.String())
	log.Printf("rootHandler %s %s", r.Method, r.URL.String())
	if r.Method != http.MethodGet {
		http.Error(w, "wrong method", http.StatusMethodNotAllowed)
		return
	}
	if r.URL.Path != "/" {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}

	ctx := r.Context()

	// get services in all the namespaces by omitting namespace
	// Or specify namespace to get pods in particular namespace
	services, err := s.services.list(ctx, 0)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	// sort on the (namespace, name) pair
	sort.Slice(services.Items, func(i, j int) bool {
		if services.Items[i].Namespace != services.Items[j].Namespace {
			return services.Items[i].Namespace < services.Items[j].Namespace
		}
		return services.Items[i].Name < services.Items[j].Name
	})

	data := &rootTemplateData{}
	lastNamespace := ""
	for _, s := range services.Items {
		if s.Namespace != lastNamespace {
			data.Namespaces = append(data.Namespaces, namespaceTemplateData{
				Name: s.Namespace,
			})
			lastNamespace = s.Namespace
		}

		lastNSData := &data.Namespaces[len(data.Namespaces)-1]

		tcpPorts := []portTemplateData{}
		for _, p := range s.Spec.Ports {
			if p.Protocol == corev1.ProtocolTCP {
				tcpPorts = append(tcpPorts, portTemplateData{p.Name, int(p.Port)})
			}
		}
		lastNSData.Services = append(lastNSData.Services, serviceTemplateData{
			Name:      s.Name,
			ClusterIP: s.Spec.ClusterIP,
			TCPPorts:  tcpPorts,
		})
	}

	err = rootTemplate.Execute(w, data)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
}

// proxies a request
func (s *server) proxyErrWrapper(w http.ResponseWriter, r *http.Request) {
	log.Printf("proxy %s %s", r.Method, r.URL.String())
	err := s.proxy(w, r)
	if err != nil {
		if errors.IsNotFound(err) {
			http.NotFound(w, r)
		} else {
			log.Printf("proxy error: %s", err.Error())
			http.Error(w, err.Error(), http.StatusInternalServerError)
		}
	}
}

func (s *server) proxy(w http.ResponseWriter, r *http.Request) error {
	matches := servicePattern.FindStringSubmatch(r.URL.Path)
	if len(matches) != 5 {
		return fmt.Errorf("bad path: %s", r.URL.Path)
	}
	namespace, service, port, destPath := matches[1], matches[2], matches[3], matches[4]
	log.Printf("ns=%s service=%s port=%s destPath=%s", namespace, service, port, destPath)

	parsedPort, err := strconv.ParseInt(port, 10, 32)
	if err != nil {
		return err
	}

	ctx := r.Context()
	serviceMeta, err := s.services.get(ctx, namespace, service)
	if err != nil {
		return err
	}

	// make sure a matching TCP port exists
	found := false
	for _, p := range serviceMeta.Spec.Ports {
		if p.Port == int32(parsedPort) && p.Protocol == corev1.ProtocolTCP {
			found = true
			break
		}
	}
	if !found {
		return fmt.Errorf("port %d not found", parsedPort)
	}

	r.URL.Scheme = "http"
	r.URL.Host = fmt.Sprintf("%s:%d", serviceMeta.Spec.ClusterIP, parsedPort)
	r.URL.Path = destPath
	log.Printf("proxying to %s", r.URL.String())

	// bit of a hack: store the original request data in the request context so the ReverseProxy
	// response rewriter can access it
	origData := origRequestData{namespace, service, parsedPort, destPath}
	rCtxWithData := context.WithValue(r.Context(), origRequestDataContextKey{}, origData)
	r2 := r.WithContext(rCtxWithData)

	s.reverseProxy.ServeHTTP(w, r2)
	return nil
}

func (s *server) proxyRewriter(resp *http.Response) error {
	origData, ok := resp.Request.Context().Value(origRequestDataContextKey{}).(origRequestData)
	if !ok {
		return fmt.Errorf("proxy error: original request data not found in context")
	}
	rootPath := fmt.Sprintf("/%s/%s/%d", origData.namespace, origData.service, origData.port)

	// rewrite the location header
	const locationHeader = "Location"
	if resp.Header.Get(locationHeader) != "" {
		newLocation := rewriteURL(resp.Header.Get(locationHeader), rootPath)
		log.Printf("proxy rewrote Location: %#v -> %#v", resp.Header.Get(locationHeader), newLocation)
		resp.Header.Set(locationHeader, newLocation)
	}

	// TODO: check params for charset
	mediaType, _, err := mime.ParseMediaType(resp.Header.Get("Content-Type"))
	if err != nil {
		log.Printf("warning: could not parse Content-Type: %s = %s; not rewriting links",
			resp.Header.Get("Content-Type"), err.Error())
	}
	if mediaType != htmlMediaType {
		return nil
	}

	// Proxying an HTML document: rewrite links so they work
	// TODO: it would be better to use a wildcard domain to put the namespace/service name
	// into the incoming URL, since relative links would then "just work" without rewriting.
	// However, Google Cloud Ingress's automatic TLS certificates do not support wildcard domains,
	// so let's write a bit more code to make this easier to use

	log.Printf("rewriting HTML paths to root=%s", rootPath)

	buf := &bytes.Buffer{}
	err = rewriteAbsolutePathLinks(buf, resp.Body, rootPath)
	if err != nil {
		return err
	}

	resp.Header.Del("Content-Length")
	resp.Body = io.NopCloser(buf)
	return nil
}

// Rewrites URL string so that any absolute path references are based on rootPath.
func rewriteURL(urlString string, rootPath string) string {
	u, err := url.Parse(urlString)
	if err != nil {
		log.Printf("warning: skipping invalid URL: %s: %s", urlString, err.Error())
		return urlString
	}
	if u.IsAbs() {
		// absolute links to other hosts
		return urlString
	}
	if u.Path == "" {
		// links with anchors "#anchor", probably others like queries "?k=v"
		return urlString
	}
	if u.Path[0] != '/' {
		// relative path references: do not rewrite
		// TODO: we might need to rewrite "../" references that could go back too far but skip for now
		return urlString
	}

	origPath := u.Path
	u.Path = path.Join(rootPath, u.Path)

	// ensure we keep the same trailing slashes
	if origPath[len(origPath)-1] == '/' && u.Path[len(u.Path)-1] != '/' {
		u.Path += "/"
	}
	return u.String()
}

// maps tag to URL attribute that should be rewritten by rewriteRelativeLinks
var attrRewrites = map[atom.Atom]string{
	atom.A:    "href",
	atom.Form: "action",
}

// Rewrites all absolute paths in the HTML document in r to start with rootPath.
func rewriteAbsolutePathLinks(w io.Writer, r io.Reader, rootPath string) error {
	tokenizer := html.NewTokenizer(r)
	for {
		tokenType := tokenizer.Next()
		if tokenType == html.ErrorToken {
			if tokenizer.Err() == io.EOF {
				break
			}
			return tokenizer.Err()
		}
		t := tokenizer.Token()
		rewriteAttr := attrRewrites[t.DataAtom]
		if rewriteAttr != "" {
			for i, attr := range t.Attr {
				if attr.Key == rewriteAttr {
					newURL := rewriteURL(attr.Val, rootPath)
					log.Printf("rewriting %s.%s=%#v -> %#v", t.DataAtom, attr.Key, attr.Val, newURL)
					t.Attr[i].Val = newURL
				}
			}
		}

		// t.String() incorrectly escapes <script> content
		// https://github.com/golang/go/issues/7929
		var content string
		if tokenType == html.TextToken {
			content = t.Data
		} else {
			content = t.String()
		}
		_, err := w.Write([]byte(content))
		if err != nil {
			return err
		}
	}
	return nil
}

func (s *server) makeSecureHandler(iapAudience string) http.Handler {
	insecureMux := http.NewServeMux()
	insecureMux.HandleFunc("/", s.rootHandler)
	insecureMux.HandleFunc("/health", s.healthHandler)
	secureMux := iap.Required(iapAudience, insecureMux)

	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if isRootHealthCheck(r) || r.URL.Path == "/health" {
			s.healthHandler(w, r)
			return
		}

		secureMux.ServeHTTP(w, r)
	})
	return handler
}

func main() {
	// https://cloud.google.com/iap/docs/signed-headers-howto#verifying_the_jwt_payload
	iapAudience := flag.String("iapAudience", "", "Identity-Aware Proxy audience (aud) field (REQUIRED)")
	flag.Parse()

	// connect to the Kubernetes APIS
	config, err := rest.InClusterConfig()
	if err != nil {
		panic(err)
	}
	clientset, err := kubernetes.NewForConfig(config)
	if err != nil {
		panic(err)
	}

	// crash early if we do not have the correct permission
	// TODO: This is probably bad: we will crash on startup if the master is down, but it
	// does make it easier to debug permissions errors. Figure out a better option?
	s := newServer(&kubernetesAPIClient{clientset})
	err = s.checkPermissions(context.Background())
	if err != nil {
		panic(err)
	}

	secureHandler := s.makeSecureHandler(*iapAudience)

	port := os.Getenv(portEnvVar)
	if port == "" {
		port = defaultPort
		log.Printf("warning: %s not specified; using default %s", portEnvVar, port)
	}

	addr := ":" + port
	log.Printf("listen addr %s (http://localhost:%s/)", addr, port)
	if err := http.ListenAndServe(addr, secureHandler); err != nil {
		panic(err)
	}
}

type rootTemplateData struct {
	Namespaces []namespaceTemplateData
}

type namespaceTemplateData struct {
	Name     string
	Services []serviceTemplateData
}

type portTemplateData struct {
	Name string
	Port int
}

type serviceTemplateData struct {
	Name      string
	ClusterIP string
	TCPPorts  []portTemplateData
}

var rootTemplate = template.Must(template.New("root").Parse(`<!doctype html>
<html>
<head><title>Kube Web Proxy</title></head>
<body>
<h1>Kube Web Proxy</h1>
<p>Proxies requests into a Kubernetes cluster.</p>
<h2>WARNING: This can be a dangerous security hole</h2>

{{range $namespace := .Namespaces}}
<h2>Namespace {{$namespace.Name}}</h2>
<ul>
{{range $service := $namespace.Services}}
<li>{{$service.Name}} 
	{{if $service.TCPPorts}}
		<em>TCP Ports</em>: 
		{{range $port := $service.TCPPorts}}
			[<a href="/{{$namespace.Name}}/{{$service.Name}}/{{$port.Port}}/">{{$port.Name}} {{$port.Port}}</a>]
		{{end}}
	{{end}}</li>
{{end}}
</ul>
{{end}}

</body>
</html>`))
