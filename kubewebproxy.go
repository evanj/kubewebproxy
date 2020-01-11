package main

import (
	"html/template"
	"log"
	"net/http"
	"os"
	"sort"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
)

const portEnvVar = "PORT"
const defaultPort = "8080"

type server struct {
	clientset *kubernetes.Clientset
}

func (s *server) checkPermissions() error {
	// attempt to list a single service to see if we have permission
	_, err := s.clientset.CoreV1().Services("").List(metav1.ListOptions{Limit: 1})
	return err
}

func (s *server) rootHandler(w http.ResponseWriter, r *http.Request) {
	log.Printf("rootHandler %s %s", r.Method, r.URL.String())
	if r.Method != http.MethodGet {
		http.Error(w, "wrong method", http.StatusMethodNotAllowed)
		return
	}
	if r.URL.Path != "/" {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}

	// ns := namespaceTemplateData{"example_namespace", []serviceTemplateData{{"example_service"}}}
	// data := &rootTemplateData{
	// 	[]namespaceTemplateData{ns},
	// }

	// get services in all the namespaces by omitting namespace
	// Or specify namespace to get pods in particular namespace
	services, err := s.clientset.CoreV1().Services("").List(metav1.ListOptions{})
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

func main() {
	// creates the in-cluster config
	config, err := rest.InClusterConfig()
	if err != nil {
		panic(err)
	}
	// creates the clientset
	clientset, err := kubernetes.NewForConfig(config)
	if err != nil {
		panic(err)
	}

	// crash early if we do not have the correct permission
	// TODO: This is probably bad: we will crash on startup if the master is down, but it
	// does make it easier to debug permissions errors. Figure out a better option?
	s := &server{clientset}
	err = s.checkPermissions()
	if err != nil {
		panic(err)
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/", s.rootHandler)

	port := os.Getenv(portEnvVar)
	if port == "" {
		port = defaultPort
		log.Printf("warning: %s not specified; using default %s", portEnvVar, port)
	}

	addr := ":" + port
	log.Printf("listen addr %s (http://localhost:%s/)", addr, port)
	if err := http.ListenAndServe(addr, mux); err != nil {
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
	Name string
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
<li>{{.Name}}</li>
{{end}}
</ul>
{{end}}

</body>
</html>`))
