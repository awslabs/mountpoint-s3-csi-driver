package cloud

import (
	"fmt"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/aws/ec2metadata"
	"github.com/aws/aws-sdk-go/aws/session"
	"k8s.io/klog/v2"
)

type EC2MetadataClient func() (EC2Metadata, error)

var DefaultEC2MetadataClient = func() (EC2Metadata, error) {
	sess := session.Must(session.NewSession(&aws.Config{}))
	svc := ec2metadata.New(sess)
	return svc, nil
}

func EC2MetadataInstanceInfo(svc EC2Metadata, regionFromSession string) (*metadata, error) {
	doc, err := svc.GetInstanceIdentityDocument()
	klog.InfoS("Retrieving EC2 instance identity metadata", "regionFromSession", regionFromSession)
	if err != nil {
		return nil, fmt.Errorf("Could not get EC2 instance identity metadata: %w", err)
	}

	if len(doc.InstanceID) == 0 {
		return nil, fmt.Errorf("Could not get valid EC2 instance ID")
	}

	if len(doc.Region) == 0 {
		if len(regionFromSession) != 0 {
			doc.Region = regionFromSession
		} else {
			return nil, fmt.Errorf("Could not get valid EC2 region")
		}
	}

	if len(doc.AvailabilityZone) == 0 {
		if len(regionFromSession) != 0 {
			doc.AvailabilityZone = regionFromSession
		} else {
			return nil, fmt.Errorf("Could not get valid EC2 availability zone")
		}
	}

	instanceInfo := metadata{
		instanceID:           doc.InstanceID,
		region:               doc.Region,
		availabilityZone:     doc.AvailabilityZone,
		isEC2MetadataEnabled: true,
	}

	return &instanceInfo, nil
}
