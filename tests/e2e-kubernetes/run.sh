#!/bin/bash

# requires kubectl in $PATH
KUBECONFIG=/home/vlaad/.kube/config go test -ginkgo.vv --bucket-region="eu-west-1" --pull-request=15