# Kube Web Proxy

This is a total hack to make debugging Kubernetes services easier. You go to the proxy, and it shows you a list of pods/services in the cluster and their ports. You then click the port you want, and it connects you to that. This makes it easy to access internal debugging interfaces for your services. For example, you can access the Go pprof web UI to collect profiling information.

The current version requires requests to have been authenticated with the Google Cloud identity-aware proxy (IAP). It verifies the headers to protect against misconfiguration, since this service effectively grants elevated access to your Kubernetes cluster. Getting this set up is a bit of a pain. My advice is:

1. Set up the Ingress, without IAP turned on. The `/health` endpoint should work, buy all other paths will return forbidden.
2. Enable IAP for the Ingress. Once you get it working correctly, it should "just work", but this is a bit of a pain.

References:
* Managed Certificates on GKE: https://cloud.google.com/kubernetes-engine/docs/how-to/managed-certs
* IAP on GKE: https://cloud.google.com/iap/docs/enabling-kubernetes-howto
* Health checks (huge PITA with IAP verification since we want to move the health check to a non-standard path): https://cloud.google.com/kubernetes-engine/docs/concepts/ingress#health_checks



## Set up / using

1. Build the container image and publish it.
2. Configure IAP for your GKE cluster: https://cloud.google.com/iap/docs/enabling-kubernetes-howto
2. Edit kubewebproxy-service.yaml and set `Image` and `--iapAudience` to the correct values.
2. kubectl apply -f kubewebproxy-service.yaml to configure the ingress, service, deployment, service account, GKE managed certificates, IAP, etc. This is unlikely to work without some edits.
3. Access the ingress in your web browser.



## Service Accounts

This service needs access to the `view` ClusterRole to get access to the Kubernetes API. I followed the in-cluster configuration example to get this to work by granting it a service account with permission to view everything:

https://github.com/kubernetes/client-go/tree/master/examples/in-cluster-client-configuration


## Run Locally

docker build . --tag=kubewebproxy
docker run --rm -ti --publish=127.0.0.1:8080:8080 kubewebproxy

Open http://localhost:8080/


## Publishing

~/google-cloud-sdk/bin/gcloud builds submit . --project=gosignin-demo --substitutions=_LABEL=debug1
