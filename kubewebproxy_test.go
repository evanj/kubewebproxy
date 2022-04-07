package main

import (
	"bytes"
	"context"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"path"
	"strconv"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

const testRedirectPath = "/redirect"
const testRedirectDest = "/other"

type staticServer struct{}

func (s *staticServer) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path == testRedirectPath {
		http.Redirect(w, r, testRedirectDest, http.StatusSeeOther)
		return
	}

	// return an unusual status to ensure we are proxying it
	w.Header().Set("contex-type", "text/html; charset=UTF-8")
	w.WriteHeader(http.StatusResetContent)
	w.Write([]byte(exampleHTML))
}

type fakeKubernetesAPIClient struct {
	services corev1.ServiceList
}

func (k *fakeKubernetesAPIClient) list(ctx context.Context, limit int64) (*corev1.ServiceList, error) {
	return &k.services, nil
}
func (k *fakeKubernetesAPIClient) get(ctx context.Context, namespace string, name string) (*corev1.Service, error) {
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
	r = httptest.NewRequest(http.MethodGet, goodRoot+"subdir/", nil)
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
	expected = `"./dir/relative1"`
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

	// test a request that should be redirected
	r = httptest.NewRequest(http.MethodGet, path.Join(goodRoot, testRedirectPath), nil)
	recorder = httptest.NewRecorder()
	kwp.proxyErrWrapper(recorder, r)
	if recorder.Code != http.StatusSeeOther {
		t.Error("expected status SeeOther (303)", recorder.Code, recorder.Body.String())
	}
	expected = path.Join(goodRoot, testRedirectDest)
	location := recorder.Header().Get("Location")
	if location != expected {
		t.Errorf("Location header should be rewritten to %#v; was %#v", expected, location)
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
	type testData struct {
		input    string
		expected string
	}
	tests := []testData{
		{"https://www.example.com/path", "https://www.example.com/path"},
		{"/root", "/extra/path/root"},
		{"/root/", "/extra/path/root/"},
		{"./dir/relative", "./dir/relative"},
		{"./dir/relative/", "./dir/relative/"},
		{"relative.txt", "relative.txt"},
		{"#anchor", "#anchor"},
	}
	for i, test := range tests {
		output := rewriteURL(test.input, rootPath)
		if output != test.expected {
			t.Errorf("%d: rewriteURL(%#v, %#v)=%#v; expected %#v",
				i, test.input, rootPath, output, test.expected)
		}
	}
}

func TestRewriteHTML(t *testing.T) {
	out := &bytes.Buffer{}
	err := rewriteAbsolutePathLinks(out, strings.NewReader(exampleHTML), "/extra/path")
	if err != nil {
		t.Fatal(err)
	}

	mustContain := []string{
		`"https://www.example.com/absolute"`,
		`"/extra/path/rootrelative"`,
		`"./dir/relative1"`,
		`"/extra/path/root/post"`,
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

func TestIsRootHealthCheck(t *testing.T) {
	type testCase struct {
		userAgent string
		path      string
		expected  bool
	}
	testCases := []testCase{
		{"kube-probe/1.13+", "/", true},
		{"kube-probe/1.17", "/", true},
		{"GoogleHC/1.0", "/", true},
		{"xGoogleHC/2.1", "/", true},

		{"GoogleHCx/2.1", "/", false},
		// real chrome
		{"Mozilla/5.0 (Macintosh; Intel Mac OS X 10_14_6) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/79.0.3945.117 Safari/537.36",
			"/", false},
		{"GoogleHC/1.0", "/other", false},
	}
	for i, test := range testCases {
		r := httptest.NewRequest(http.MethodGet, test.path, nil)
		r.Header.Set("User-Agent", test.userAgent)
		result := isRootHealthCheck(r)
		if result != test.expected {
			t.Errorf("%d: isRootHealthCheck(User-Agent:%s Path:%s)=%t; expected %t",
				i, test.userAgent, test.path, result, test.expected)
		}
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
