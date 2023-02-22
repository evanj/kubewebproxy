# Kube Web Proxy

WARNING: I wrote this long ago. It may be useful as a proof of concept. I have periodically made sure it stil builds, but I suspect it will not work when tested next.

This is a hack to make it easy to connect to HTTP debug interfaces inside your Kubernetes cluster. You go to the proxy, and it shows you a list of pods/services in the cluster. You click the port you want, and it connects you. This makes it easy to access internal debugging interfaces. For example, you can access the [Go pprof web UI](https://golang.org/pkg/net/http/pprof/) to collect profiling information. This is a bit like the port-forwarding part of [Octant](https://github.com/vmware-tanzu/octant), but runs in your cluster so it doesn't require installing anything, and it is very, very ugly.

![Screenshot](./kubewebproxy-screenshot.png)

Since this is a potential security hole, Kube Web Proxy requires requests to have been authenticated with the [Google Cloud Identity-Aware Proxy (IAP)](https://cloud.google.com/iap/). Getting this set up is a bit of a pain. My advice is:

1. Set up the Ingress, without IAP turned on. The `/health` endpoint should work, but the other paths will return forbidden errors.
2. Enable IAP for the Ingress, which may require some fiddling.


## Limitations

The proxy has to rewrite paths, in order to add `/namespace/service/port` to the URL path. I have only used a few tiny web applications, so I'm certainly missing some rewrites that are required. Cookies will be shared between all services (TODO: rewrite the header to scope them to paths). This also means JavaScript that embeds paths will probably break. The solution is probably to rewrite the application to use relative paths. A better solution would be to use a wildcard domain, but that is not supported by Google's managed TLS certificates.


## Useful Documentation
* [Managed Certificates on GKE](https://cloud.google.com/kubernetes-engine/docs/how-to/managed-certs)
* [IAP on GKE](https://cloud.google.com/iap/docs/enabling-kubernetes-howto)
* [GKE Ingress Health checks](https://cloud.google.com/kubernetes-engine/docs/concepts/ingress#health_checks) Verifying IAP headers requires configuring the health check to use a path other than `/`



## Set up / using

1. Build the container image and publish it.
2. Configure IAP for your GKE cluster: https://cloud.google.com/iap/docs/enabling-kubernetes-howto. Most importantly, go to the [OAuth Credentals page](https://console.cloud.google.com/apis/credentials) and get the Client ID and secret, then create the required Kubernetes secret: `kubectl create secret generic iap-secret --from-literal=client_id=xxx.googleusercontent.com --from-literal=client_secret=...`
3. Edit kubewebproxy-deploy.yaml and set `Image` to the value you published, and configure the Ingress host name to the name you want, then apply it.
4. `kubectl describe ingress kubewebproxy` and get the IP address. Configure your DNS for the host name you want to point to it.
5. Go to the IAP page, and copy the Signed Header JWT Audience for the ingress.
6. Put it in the `--iapAudience` flag in `kubewebproxy-deploy.yaml` and apply it again.
7. Access the ingress in your web browser. It should ideally work. First test the /health endpoint since it is public.

*Note*: Health checking on a custom path is a huge pain. The pod needs to exist before the ingress, so I had to delete and re-create the ingress to get it to work.



## Kubernetes Service Account

This service the `view` ClusterRole to read service metadata from the Kubernetes API. I followed the in-cluster configuration example to grant it a service account with permission to view everything:

https://github.com/kubernetes/client-go/tree/master/examples/in-cluster-client-configuration


## Run Locally

docker build . --tag=kubewebproxy
docker run --rm -ti --publish=127.0.0.1:8080:8080 kubewebproxy

Open http://localhost:8080/


## Building with Google Cloud Build

~/google-cloud-sdk/bin/gcloud builds submit . --project=gosignin-demo --substitutions=_LABEL=debug1
