# Kube Web Proxy

This is a total hack to proxy web requests into a Kubernetes cluster to make debugging easier.

TODO: Integrate Google Cloud IAP. Setting that up for an ingress is a PITA:

https://cloud.google.com/iap/docs/enabling-kubernetes-howto


## Using

1. Build the container image and publish it somewhere.
2. kubectl apply -f kubewebproxy-service.yaml to configure the ingress, service, and the code. WARNING: This will make view access to your cluster PUBLIC.
3. Access the ingress!

## Service Accounts

This service needs access to the `view` ClusterRole to get access to the Kubernetes API. I followed the in-cluster configuration example to get this to work by granting it a service account to view everything:

https://github.com/kubernetes/client-go/tree/master/examples/in-cluster-client-configuration


## Run Locally

docker build . --tag=kubewebproxy
docker run --rm -ti --publish=127.0.0.1:8080:8080 pprofweb

Open http://localhost:8080/


## Publishing

~/google-cloud-sdk/bin/gcloud builds submit . --project=gosignin-demo --substitutions=_LABEL=debug1
