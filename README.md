# Terminate Sidecar Job Controller

This is a controller that kills the istio-proxy sidecar in pods created by jobs.
It does this by watching all the pods and handling pods created by jobs,
checking when the main container is terminated and killing the istio-proxy
sidecar by "execing" into the pod and killing the main process.

It is based on the project here - https://github.com/nrmitchi/k8s-controller-sidecars
But implemented in Go 1.20