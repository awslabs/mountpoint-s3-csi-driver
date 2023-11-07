module github.com/awslabs/aws-s3-csi-driver

go 1.21

require (
	github.com/aws/aws-sdk-go v1.45.13
	github.com/container-storage-interface/spec v1.8.0
	github.com/coreos/go-systemd/v22 v22.5.0
	github.com/godbus/dbus/v5 v5.1.0
	github.com/golang/mock v1.6.0
	github.com/kubernetes-csi/csi-test v2.2.0+incompatible
	github.com/onsi/ginkgo v1.16.5
	github.com/onsi/gomega v1.27.6
	google.golang.org/grpc v1.59.0
	k8s.io/klog/v2 v2.100.1
	k8s.io/mount-utils v0.28.2
)

require (
	github.com/kr/text v0.2.0 // indirect
	google.golang.org/genproto/googleapis/rpc v0.0.0-20230822172742-b8732ec3820d // indirect
)

require (
	github.com/fsnotify/fsnotify v1.4.9 // indirect
	github.com/go-logr/logr v1.2.4 // indirect
	github.com/golang/protobuf v1.5.3 // indirect
	github.com/google/go-cmp v0.5.9 // indirect
	github.com/google/uuid v1.3.1
	github.com/jmespath/go-jmespath v0.4.0 // indirect
	github.com/moby/sys/mountinfo v0.6.2 // indirect
	github.com/nxadm/tail v1.4.8 // indirect
	golang.org/x/net v0.17.0
	golang.org/x/sys v0.13.0
	golang.org/x/text v0.13.0 // indirect
	google.golang.org/protobuf v1.31.0
	gopkg.in/tomb.v1 v1.0.0-20141024135613-dd632973f1e7 // indirect
	gopkg.in/yaml.v2 v2.4.0 // indirect
	gopkg.in/yaml.v3 v3.0.1 // indirect
	k8s.io/apimachinery v0.28.2
	k8s.io/utils v0.0.0-20230406110748-d93618cff8a2 // indirect
)
