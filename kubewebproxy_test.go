package main

import (
	"bytes"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
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

func TestRoot(t *testing.T) {
	f := &fakeKubernetesAPIClient{}
	f.services.Items = append(f.services.Items, corev1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: "namespace",
			Name:      "service",
		},
		Spec: corev1.ServiceSpec{
			Ports: []corev1.ServicePort{{
				Name:     "portname",
				Protocol: corev1.ProtocolTCP,
				Port:     123,
			}},
		},
	})
	s := newServer(f)

	r := httptest.NewRequest(http.MethodGet, "/", nil)
	recorder := httptest.NewRecorder()
	s.rootHandler(recorder, r)
	if recorder.Code != http.StatusOK {
		t.Error("expected status OK", recorder.Code)
	}
	if strings.Contains(recorder.Body.String(), "template:") {
		t.Error("found template error in output")
		t.Error(recorder.Body.String())
	}
	if !strings.Contains(recorder.Body.String(), `href="/namespace/service/123/"`) {
		t.Error("should have found link")
		t.Error(recorder.Body.String())
	}
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
			ClusterIP: "localhost",
			Ports: []corev1.ServicePort{{
				Protocol: corev1.ProtocolUDP,
				Name:     "udp-donotdisplay",
				Port:     int32(testServerAddr.Port),
			}, {
				Protocol: corev1.ProtocolTCP,
				Port:     int32(testServerAddr.Port),
			}},
		},
	})
	kwp := newServer(fakeAPI)

	r, err := http.NewRequest(http.MethodGet, "/namespace/notfound/123/", nil)
	if err != nil {
		t.Fatal(err)
	}
	recorder := httptest.NewRecorder()
	kwp.proxyErrWrapper(recorder, r)
	if recorder.Code != http.StatusNotFound {
		t.Error("expected status NotFound", recorder.Code, recorder.Body.String())
	}

	goodRoot := fmt.Sprintf("/namespace/service/%d/", testServerAddr.Port)
	r, err = http.NewRequest(http.MethodGet, goodRoot+"subdir/", nil)
	if err != nil {
		t.Fatal(err)
	}
	recorder = httptest.NewRecorder()
	kwp.proxyErrWrapper(recorder, r)
	if recorder.Code != http.StatusResetContent {
		t.Error("expected status ResetContent (205)", recorder.Code, recorder.Body.String())
	}
	expected := fmt.Sprintf(`"%srootrelative"`, goodRoot)
	if !strings.Contains(recorder.Body.String(), expected) {
		t.Errorf("output should contain %#v", expected)
		t.Error(recorder.Body.String())
	}
	expected = fmt.Sprintf(`"%ssubdir/dir/relative1"`, goodRoot)
	if !strings.Contains(recorder.Body.String(), expected) {
		t.Errorf("output should contain %#v", expected)
		t.Error(recorder.Body.String())
	}

	// ensure content-length was rewritten correctly
	if recorder.Header().Get("Content-Length") != "" {
		contentLength, err := strconv.Atoi(recorder.Header().Get("Content-Length"))
		if err != nil {
			t.Fatal(err)
		}
		if contentLength != recorder.Body.Len() {
			t.Errorf("Content-Length=%d; Body length was %d: must match",
				contentLength, recorder.Body.Len())
		}
	}
}

func TestPathRegexp(t *testing.T) {
	matches := servicePattern.FindStringSubmatch("/namespace/service/123/")
	if len(matches) != 5 {
		t.Error(matches)
	}
	if !(matches[1] == "namespace" && matches[2] == "service" && matches[3] == "123" && matches[4] == "/") {
		t.Error(matches)
	}
}

func TestRewriteURL(t *testing.T) {
	const rootPath = "/extra/path"
	const relativePath = "/subdir/"
	type testData struct {
		input    string
		expected string
	}
	tests := []testData{
		{"https://www.example.com/path", "https://www.example.com/path"},
		{"/root", "/extra/path/root"},
		{"/root/", "/extra/path/root/"},
		{"./dir/relative", "/extra/path/subdir/dir/relative"},
		{"./dir/relative/", "/extra/path/subdir/dir/relative/"},
		{"relative.txt", "/extra/path/subdir/relative.txt"},
		{"#anchor", "#anchor"},
	}
	for i, test := range tests {
		output := rewriteURL(test.input, rootPath, relativePath)
		if output != test.expected {
			t.Errorf("%d: rewriteURL(%#v, %#v, %#v)=%#v; expected %#v",
				i, test.input, rootPath, relativePath, output, test.expected)
		}
	}
}

func TestRewriteHTML(t *testing.T) {
	out := &bytes.Buffer{}
	err := rewriteRelativeLinks(out, strings.NewReader(exampleHTML), "/extra/path", "/subdir")
	if err != nil {
		t.Fatal(err)
	}

	mustContain := []string{
		`"https://www.example.com/absolute"`,
		`"/extra/path/rootrelative"`,
		`"/extra/path/subdir/dir/relative1"`,
		// Checks for https://github.com/golang/go/issues/7929
		`"use strict";`,
	}
	for i, s := range mustContain {
		if !strings.Contains(out.String(), s) {
			t.Errorf("%d: output must contain %#v\n%s", i, s, out.String())
		}
	}
}

func TestHealth(t *testing.T) {
	fakeAPI := &fakeKubernetesAPIClient{}
	kwp := newServer(fakeAPI)
	handler := kwp.makeSecureHandler("noaudience")

	req := httptest.NewRequest(http.MethodGet, "/health", nil)
	resp := httptest.NewRecorder()
	handler.ServeHTTP(resp, req)
	if resp.Code != http.StatusOK {
		t.Error("health check should return 200 OK", resp.Code)
	}
}

const exampleHTML = `<html><body>
<a href="./dir/relative1">relative1</a>
<a href="/rootrelative">rootrelative</a>
<a href="https://www.example.com/absolute">absolute</a>
<form method="post" action="/root/post">
<script>
function initPanAndZoom(svg, clickHandler) {
	// x/net/html has a bug when printing scripts: https://github.com/golang/go/issues/7929
  "use strict";
}
</script>
</body></html>`
