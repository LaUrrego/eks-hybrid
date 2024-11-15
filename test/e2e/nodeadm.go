//go:build e2e
// +build e2e

package e2e

import (
	"context"
	"fmt"
	"net/url"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/s3"
	"github.com/aws/aws-sdk-go/service/ssm"
	"github.com/aws/eks-hybrid/internal/api"
	"github.com/aws/eks-hybrid/internal/creds"
	"github.com/go-logr/logr"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

const ssmActivationName = "eks-hybrid-ssm-provider"

// NodeadmOS defines an interface for operating system-specific behavior.
type NodeadmOS interface {
	Name() string
	AMIName() string
	BuildUserData(nodeadmUrl, nodeadmConfigYaml, kubernetesVersion, provider string) []byte
}

type Ubuntu2204 struct{}

func (u *Ubuntu2204) Name() string {
	return "ubuntu2204"
}

func (u *Ubuntu2204) AMIName() string {
	return "/aws/service/canonical/ubuntu/server/22.04/stable/current/amd64/hvm/ebs-gp2/ami-id"
}

func (u *Ubuntu2204) BuildUserData(nodeadmUrl, nodeadmConfigYaml, kubernetesVersion, provider string) []byte {
	data := `#!/bin/bash
# download nodeadm binary
echo "Downloading nodeadm binary"
curl -L "%s" -o /tmp/nodeadm
chmod +x /tmp/nodeadm

echo "Downloading nodeadm-config.yaml"
echo '%s' > nodeadm-config.yaml

echo "Installing kubernetes components"
/tmp/nodeadm install %s --credential-provider %s

echo "Initializing the node"
/tmp/nodeadm init -c file:///nodeadm-config.yaml
`
	userdata := fmt.Sprintf(data, nodeadmUrl, nodeadmConfigYaml, kubernetesVersion, provider)
	return []byte(userdata)
}

type NodeadmCredentialsProvider interface {
	Name() creds.CredentialProvider
	NodeadmConfig(cluster *hybridCluster) (*api.NodeConfig, error)
}

type SsmProvider struct {
	ssmClient *ssm.SSM
	role      string
}

func (s *SsmProvider) Name() creds.CredentialProvider {
	return creds.SsmCredentialProvider
}

func (s *SsmProvider) NodeadmConfig(cluster *hybridCluster) (*api.NodeConfig, error) {
	ssmActivationDetails, err := createSSMActivation(s.ssmClient, s.role, ssmActivationName)
	if err != nil {
		return nil, err
	}
	return &api.NodeConfig{
		TypeMeta: metav1.TypeMeta{
			APIVersion: "node.eks.aws/v1alpha1",
			Kind:       "NodeConfig",
		},
		Spec: api.NodeConfigSpec{
			Cluster: api.ClusterDetails{
				Name:   cluster.clusterName,
				Region: cluster.clusterRegion,
			},
			Hybrid: &api.HybridOptions{
				SSM: &api.SSM{
					ActivationID:   *ssmActivationDetails.ActivationId,
					ActivationCode: *ssmActivationDetails.ActivationCode,
				},
			},
		},
	}, nil
}

func parseS3URL(s3URL string) (bucket, key string, err error) {
	parsedURL, err := url.Parse(s3URL)
	if err != nil {
		return "", "", err
	}

	parts := strings.SplitN(parsedURL.Host, ".", 2)
	if len(parts) < 2 {
		return "", "", fmt.Errorf("invalid S3 URL format")
	}
	bucket = parts[0]
	key = strings.TrimPrefix(parsedURL.Path, "/")
	return bucket, key, nil
}

func generatePreSignedURL(client *s3.S3, bucket, key string, expiration time.Duration) (string, error) {
	req, _ := client.GetObjectRequest(&s3.GetObjectInput{
		Bucket: aws.String(bucket),
		Key:    aws.String(key),
	})

	url, err := req.Presign(expiration)
	if err != nil {
		return "", fmt.Errorf("generating pre-signed URL: %v", err)
	}
	return url, nil
}

func getNodeadmURL(client *s3.S3, nodeadmUrl string) (string, error) {
	s3Bucket, s3BucketKey, err := parseS3URL(nodeadmUrl)
	if err != nil {
		return "", fmt.Errorf("parsing S3 URL: %v", err)
	}

	preSignedURL, err := generatePreSignedURL(client, s3Bucket, s3BucketKey, 15*time.Minute)
	if err != nil {
		return "", fmt.Errorf("getting presigned URL for nodeadm: %v", err)
	}
	return preSignedURL, nil
}

func runNodeadmUninstall(ctx context.Context, client *ssm.SSM, instanceID string, logger logr.Logger) error {
	commands := []string{
		// TODO: @pjshah run uninstall without node-validation and pod-validation flags after adding cordon and drain node functionality
		"sudo ./nodeadm uninstall -skip node-validation,pod-validation",
	}
	ssmConfig := &ssmConfig{
		client:     client,
		instanceID: instanceID,
		commands:   commands,
	}
	outputs, err := ssmConfig.runCommandsOnInstance(ctx, logger)
	if err != nil {
		return fmt.Errorf("running SSM command: %w", err)
	}
	logger.Info("Nodeadm Uninstall", "output", outputs)
	for _, output := range outputs {
		if *output.Status != "Success" {
			logger.Info("Ignore the above ssm command failure for now if the credential provider is SSM.")
			return nil
		}
	}
	return nil
}
