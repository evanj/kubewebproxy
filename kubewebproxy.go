package main

import (
	"flag"
	"fmt"
	"html/template"
	"io"
	"log"
	"mime"
	"net/http"
	"net/url"
	"os"
	"path"
	"regexp"
	"sort"

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

var servicePattern = regexp.MustCompile(`^/([^/]+)/([^/]+)(.*)$`)

type serviceInfo interface {
	list(limit int64) (*corev1.ServiceList, error)
	get(namespace string, name string) (*corev1.Service, error)
}

type kubernetesAPIClient struct {
	clientset *kubernetes.Clientset
}

func (k *kubernetesAPIClient) list(limit int64) (*corev1.ServiceList, error) {
	return k.clientset.CoreV1().Services("").List(metav1.ListOptions{Limit: limit})
}
func (k *kubernetesAPIClient) get(namespace string, name string) (*corev1.Service, error) {
	return k.clientset.CoreV1().Services(namespace).Get(name, metav1.GetOptions{})
}

type server struct {
	services serviceInfo
}

func (s *server) checkPermissions() error {
	// attempt to list a single service to see if we have permission
	_, err := s.services.list(1)
	return err
}

func (s *server) healthHandler(w http.ResponseWriter, r *http.Request) {
	log.Printf("health check URL=%s RemoteAddr=%s UserAgent=%s",
		r.URL.String(), r.RemoteAddr, r.UserAgent())
	w.Header().Set("Content-Type", "text/plain;charset=utf-8")
	w.Write([]byte("ok\n"))
}

func (s *server) rootHandler(w http.ResponseWriter, r *http.Request) {
	email := iap.Email(r)
	log.Printf("rootHandler user=%s %s %s", email, r.Method, r.URL.String())
	if r.Method != http.MethodGet {
		http.Error(w, "wrong method", http.StatusMethodNotAllowed)
		return
	}
	if r.URL.Path != "/" {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}

	// get services in all the namespaces by omitting namespace
	// Or specify namespace to get pods in particular namespace
	services, err := s.services.list(0)
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
		lastNSData.Services = append(lastNSData.Services, serviceTemplateData{
			Name: s.Name,
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
	if r.Method != http.MethodGet {
		http.Error(w, "wrong method", http.StatusMethodNotAllowed)
		return
	}
	err := s.proxy(w, r)
	if err != nil {
		log.Printf("WTF %s", err)
		if errors.IsNotFound(err) {
			http.NotFound(w, r)
		} else {
			http.Error(w, err.Error(), http.StatusInternalServerError)
		}
	}
}

func firstTCPPort(s *corev1.Service) (corev1.ServicePort, bool) {
	for _, p := range s.Spec.Ports {
		if p.Protocol == corev1.ProtocolTCP {
			return p, true
		}
	}
	return corev1.ServicePort{}, false
}

func (s *server) proxy(w http.ResponseWriter, r *http.Request) error {
	matches := servicePattern.FindStringSubmatch(r.URL.Path)
	if len(matches) != 4 {
		return fmt.Errorf("bad path: %s", r.URL.Path)
	}
	namespace, service, path := matches[1], matches[2], matches[3]
	log.Printf("ns=%s service=%s path=%s", namespace, service, path)

	serviceMeta, err := s.services.get(namespace, service)
	log.Printf("x, %s %s", serviceMeta, err)
	if err != nil {
		return err
	}
	log.Printf("1, %s", serviceMeta)

	tcpPort, ok := firstTCPPort(serviceMeta)
	if !ok {
		return fmt.Errorf("no TCP ports found")
	}

	urlString := fmt.Sprintf("http://%s:%d%s", serviceMeta.Spec.ClusterIP, tcpPort.Port, path)
	log.Printf("proxying to %s", urlString)
	// TODO: use httputil.ReverseProxy?
	outR, err := http.NewRequest(http.MethodGet, urlString, nil)
	if err != nil {
		return err
	}
	resp, err := http.DefaultClient.Do(outR)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	// copy out the headers and status code
	for k, vList := range resp.Header {
		for _, v := range vList {
			w.Header().Set(k, v)
		}
	}
	w.WriteHeader(resp.StatusCode)

	// TODO: check params for charset
	mediaType, _, err := mime.ParseMediaType(resp.Header.Get("Content-Type"))
	if err != nil {
		log.Printf("warning: could not parse Content-Type: %s = %s", resp.Header.Get("Content-Type"), err.Error())
	}
	if mediaType == htmlMediaType {
		// rewrite links so they work
		// TODO: it would be better to use a wildcard domain to put the namespace/service name
		// into the incoming URL, since relative links would then "just work" without rewriting.
		// However, Google Cloud Ingress's automatic TLS certificates do not support wildcard domains,
		// so let's write a bit more code to make this easier to use
		pathPrefix := fmt.Sprintf("/%s/%s", namespace, service)
		log.Printf("rewriting relative URLs to add %s", pathPrefix)
		err = rewriteRelativeLinks(w, resp.Body, pathPrefix)
	} else {
		// not text/html: pass through
		_, err = io.Copy(w, resp.Body)
	}
	if err != nil {
		return err
	}
	return resp.Body.Close()
}

func rewriteURL(urlString string, pathPrefix string) string {
	u, err := url.Parse(urlString)
	if err != nil {
		log.Printf("warning: skipping invalid URL: %s: %s", urlString, err.Error())
		return urlString
	}
	// this is NOT equivalent to ResolveReference since we want to prepend the path
	if u.IsAbs() {
		return urlString
	}
	origPath := u.Path
	u.Path = path.Join(pathPrefix, u.Path)
	// ensure we keep the same trailing slashes
	if origPath[len(origPath)-1] == '/' && u.Path[len(u.Path)-1] != '/' {
		u.Path += "/"
	}
	return u.String()
}

// Rewrites all relative links in the HTML document in r to start with pathPrefix.
func rewriteRelativeLinks(w io.Writer, r io.Reader, pathPrefix string) error {
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
		if t.DataAtom == atom.A {
			for i, attr := range t.Attr {
				if attr.Key == "href" {
					t.Attr[i].Val = rewriteURL(attr.Val, pathPrefix)
				}
			}
		}

		_, err := w.Write([]byte(t.String()))
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
	return iap.RequiredWithExceptions(iapAudience, insecureMux, []string{"/health"})
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
	s := &server{&kubernetesAPIClient{clientset}}
	err = s.checkPermissions()
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

type serviceTemplateData struct {
	Name         string
	ClusterIP    string
	FirstTCPPort int
}

var rootTemplate = template.Must(template.New("root").Parse(`<!doctype html>
<html>
<head><title>Kube Web Proxy</title></head>
<body>
<h1>Kube Web Proxy</h1>
<p>Proxies requests into a Kubernetes cluster.</p>
<h2>WARNING: This can be a dangerous security hole</h2>

{{range .Namespaces}}
<h2>Namespace {{.Name}}</h2>
<ul>
{{range .Services}}
<li>{{.Name}} (IP: {{.ClusterIP}} TCP Port: {{.FirstTCPPort}})</li>
{{end}}
</ul>
{{end}}

</body>
</html>`))
