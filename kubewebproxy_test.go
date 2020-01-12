package main

import (
	"bytes"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

type staticServer struct{}

func (s *staticServer) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	// return an unusual status to ensure we are proxying it
	w.Header().Set("contex-type", "text/html; charset=UTF-8")
	w.WriteHeader(http.StatusResetContent)
	w.Write([]byte(exampleHTML))
}

type fakeKubernetesAPIClient struct {
	services corev1.ServiceList
}

func (k *fakeKubernetesAPIClient) list(limit int64) (*corev1.ServiceList, error) {
	return &k.services, nil
}
func (k *fakeKubernetesAPIClient) get(namespace string, name string) (*corev1.Service, error) {
	for _, s := range k.services.Items {
		if s.Namespace == namespace && s.Name == name {
			return &s, nil
		}
	}
	return nil, errors.NewNotFound(schema.GroupResource{}, name)
}

func TestProxy(t *testing.T) {
	static := &staticServer{}
	testServer := httptest.NewServer(static)
	defer testServer.Close()
	testServerAddr := testServer.Listener.Addr().(*net.TCPAddr)

	fakeAPI := &fakeKubernetesAPIClient{}
	fakeAPI.services.Items = append(fakeAPI.services.Items, corev1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: "namespace",
			Name:      "service",
		},
		Spec: corev1.ServiceSpec{
			Ports: []corev1.ServicePort{{
				Protocol: corev1.ProtocolTCP,
				Port:     int32(testServerAddr.Port),
			}},
		},
	})
	kwp := &server{fakeAPI}

	r, err := http.NewRequest(http.MethodGet, "/namespace/notfound/", nil)
	if err != nil {
		t.Fatal(err)
	}
	recorder := httptest.NewRecorder()
	kwp.proxyErrWrapper(recorder, r)
	if recorder.Code != http.StatusNotFound {
		t.Error("expected status NotFound", recorder.Code, recorder.Body.String())
	}

	r, err = http.NewRequest(http.MethodGet, "/namespace/service/", nil)
	if err != nil {
		t.Fatal(err)
	}
	recorder = httptest.NewRecorder()
	kwp.proxyErrWrapper(recorder, r)
	if recorder.Code != http.StatusResetContent {
		t.Error("expected status ResetContent (205)", recorder.Code, recorder.Body.String())
	}
	if !strings.Contains(recorder.Body.String(), `"/namespace/service/rootrelative"`) {
		t.Error(recorder.Body.String())
	}
}

func TestPathRegexp(t *testing.T) {
	matches := servicePattern.FindStringSubmatch("/namespace/service/")
	if len(matches) != 4 {
		t.Error(matches)
	}
	if !(matches[1] == "namespace" && matches[2] == "service" && matches[3] == "/") {
		t.Error(matches)
	}
}

func TestRewriteURL(t *testing.T) {
	const pathPrefix = "/extra/path"
	type testData struct {
		input    string
		expected string
	}
	tests := []testData{
		{"https://www.example.com/path", "https://www.example.com/path"},
		{"/root", "/extra/path/root"},
		{"/root/", "/extra/path/root/"},
		{"./dir/relative", "/extra/path/dir/relative"},
		{"relative.txt", "/extra/path/relative.txt"},
	}
	for i, test := range tests {
		output := rewriteURL(test.input, pathPrefix)
		if output != test.expected {
			t.Errorf("%d: rewriteURL(%#v, %#v)=%#v; expected %#v",
				i, test.input, pathPrefix, output, test.expected)
		}
	}
}

func TestRewriteHTML(t *testing.T) {
	out := &bytes.Buffer{}
	err := rewriteRelativeLinks(out, strings.NewReader(exampleHTML), "/extra/path")
	if err != nil {
		t.Fatal(err)
	}

	mustContain := []string{
		`"https://www.example.com/absolute"`,
		`"/extra/path/rootrelative"`,
		`"/extra/path/dir/relative1"`,
	}
	for i, s := range mustContain {
		if !strings.Contains(out.String(), s) {
			t.Errorf("%d: output must contain %#v\n%s", i, s, out.String())
		}
	}
}

const exampleHTML = `<html><body>
<a href="./dir/relative1">relative1</a>
<a href="/rootrelative">rootrelative</a>
<a href="https://www.example.com/absolute">absolute</a></body></html>`
