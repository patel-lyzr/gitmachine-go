package gitmachine

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/pem"
	"fmt"
	"os"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/ec2"
	ec2types "github.com/aws/aws-sdk-go-v2/service/ec2/types"
	"golang.org/x/crypto/ssh"
)

const (
	defaultInstanceType = "t3.medium"
	defaultSSHUser      = "ubuntu"
	defaultRegion       = "us-east-1"
	defaultAMI          = "ami-0c7217cdde317cfec" // Ubuntu 22.04 LTS us-east-1
	ec2PollInterval     = 3 * time.Second
)

// EC2Provider implements CloudProvider for AWS EC2.
type EC2Provider struct {
	ec2Client *ec2.Client

	region       string
	ami          string
	instanceType string
	keyName      string
	privateKey   string
	sgIDs        []string
	subnetID     string
	sshUser      string
	tags         map[string]string

	// Track temporary key pair for cleanup.
	createdKeyName string
}

// NewEC2Provider creates a new AWS EC2 cloud provider.
func NewEC2Provider(config *EC2MachineConfig) *EC2Provider {
	accessKey := ""
	secretKey := ""
	region := defaultRegion
	ami := defaultAMI
	instanceType := defaultInstanceType
	sshUser := defaultSSHUser
	var keyName, privateKey string
	var sgIDs []string
	var subnetID string
	var tags map[string]string

	if config != nil {
		accessKey = config.AccessKeyID
		secretKey = config.SecretAccessKey
		if config.Region != "" {
			region = config.Region
		}
		if config.AMI != "" {
			ami = config.AMI
		}
		if config.InstanceType != "" {
			instanceType = config.InstanceType
		}
		keyName = config.KeyName
		privateKey = config.PrivateKeyPEM
		sgIDs = config.SecurityGroupIDs
		subnetID = config.SubnetID
		if config.SSHUser != "" {
			sshUser = config.SSHUser
		}
		tags = config.Tags
	}

	if accessKey == "" {
		accessKey = os.Getenv("AWS_ACCESS_KEY_ID")
	}
	if secretKey == "" {
		secretKey = os.Getenv("AWS_SECRET_ACCESS_KEY")
	}
	if region == defaultRegion {
		if envRegion := os.Getenv("AWS_REGION"); envRegion != "" {
			region = envRegion
		}
	}

	ec2Client := ec2.New(ec2.Options{
		Region:      region,
		Credentials: credentials.NewStaticCredentialsProvider(accessKey, secretKey, ""),
	})

	return &EC2Provider{
		ec2Client:    ec2Client,
		region:       region,
		ami:          ami,
		instanceType: instanceType,
		keyName:      keyName,
		privateKey:   privateKey,
		sgIDs:        sgIDs,
		subnetID:     subnetID,
		sshUser:      sshUser,
		tags:         tags,
	}
}

func (p *EC2Provider) Name() string { return "aws" }

func (p *EC2Provider) Launch(ctx context.Context) (*CloudInstance, error) {
	// Create temporary key pair if none provided.
	if p.keyName == "" {
		if err := p.createKeyPair(ctx); err != nil {
			return nil, fmt.Errorf("create key pair: %w", err)
		}
	}

	// Create temporary security group if none provided.
	if len(p.sgIDs) == 0 {
		if err := p.createSecurityGroup(ctx); err != nil {
			return nil, fmt.Errorf("create security group: %w", err)
		}
	}

	input := &ec2.RunInstancesInput{
		ImageId:          aws.String(p.ami),
		InstanceType:     ec2types.InstanceType(p.instanceType),
		KeyName:          aws.String(p.keyName),
		SecurityGroupIds: p.sgIDs,
		MinCount:         aws.Int32(1),
		MaxCount:         aws.Int32(1),
	}
	if p.subnetID != "" {
		input.SubnetId = aws.String(p.subnetID)
	}

	allTags := []ec2types.Tag{{
		Key:   aws.String("Name"),
		Value: aws.String("gitmachine"),
	}}
	for k, v := range p.tags {
		allTags = append(allTags, ec2types.Tag{
			Key:   aws.String(k),
			Value: aws.String(v),
		})
	}
	input.TagSpecifications = []ec2types.TagSpecification{{
		ResourceType: ec2types.ResourceTypeInstance,
		Tags:         allTags,
	}}

	result, err := p.ec2Client.RunInstances(ctx, input)
	if err != nil {
		return nil, fmt.Errorf("RunInstances: %w", err)
	}
	if len(result.Instances) == 0 {
		return nil, fmt.Errorf("no instances returned")
	}

	instanceID := *result.Instances[0].InstanceId

	// Wait for running.
	if err := p.waitForState(ctx, instanceID, ec2types.InstanceStateNameRunning); err != nil {
		return nil, err
	}

	// Get public IP.
	inst, err := p.describe(ctx, instanceID)
	if err != nil {
		return nil, err
	}

	return inst, nil
}

func (p *EC2Provider) StopInstance(ctx context.Context, id string) error {
	_, err := p.ec2Client.StopInstances(ctx, &ec2.StopInstancesInput{
		InstanceIds: []string{id},
	})
	if err != nil {
		return fmt.Errorf("StopInstances: %w", err)
	}
	return p.waitForState(ctx, id, ec2types.InstanceStateNameStopped)
}

func (p *EC2Provider) StartInstance(ctx context.Context, id string) (*CloudInstance, error) {
	_, err := p.ec2Client.StartInstances(ctx, &ec2.StartInstancesInput{
		InstanceIds: []string{id},
	})
	if err != nil {
		return nil, fmt.Errorf("StartInstances: %w", err)
	}

	if err := p.waitForState(ctx, id, ec2types.InstanceStateNameRunning); err != nil {
		return nil, err
	}

	return p.describe(ctx, id)
}

func (p *EC2Provider) Terminate(ctx context.Context, id string) error {
	_, err := p.ec2Client.TerminateInstances(ctx, &ec2.TerminateInstancesInput{
		InstanceIds: []string{id},
	})
	if err != nil {
		_ = err // best effort
	}

	// Clean up temporary key pair (SG is shared and kept).
	if p.createdKeyName != "" {
		_, _ = p.ec2Client.DeleteKeyPair(ctx, &ec2.DeleteKeyPairInput{
			KeyName: aws.String(p.createdKeyName),
		})
	}

	return nil
}

func (p *EC2Provider) Describe(ctx context.Context, id string) (*CloudInstance, error) {
	return p.describe(ctx, id)
}

func (p *EC2Provider) SSHConfig() (user string, privateKeyPEM string) {
	return p.sshUser, p.privateKey
}

// --- Internal helpers ---

func (p *EC2Provider) describe(ctx context.Context, id string) (*CloudInstance, error) {
	desc, err := p.ec2Client.DescribeInstances(ctx, &ec2.DescribeInstancesInput{
		InstanceIds: []string{id},
	})
	if err != nil {
		return nil, fmt.Errorf("DescribeInstances: %w", err)
	}
	if len(desc.Reservations) == 0 || len(desc.Reservations[0].Instances) == 0 {
		return nil, fmt.Errorf("instance %s not found", id)
	}

	inst := desc.Reservations[0].Instances[0]
	ci := &CloudInstance{ID: id}
	if inst.PublicIpAddress != nil {
		ci.PublicIP = *inst.PublicIpAddress
	}
	return ci, nil
}

func (p *EC2Provider) waitForState(ctx context.Context, id string, target ec2types.InstanceStateName) error {
	for {
		desc, err := p.ec2Client.DescribeInstances(ctx, &ec2.DescribeInstancesInput{
			InstanceIds: []string{id},
		})
		if err != nil {
			return fmt.Errorf("describe instance: %w", err)
		}
		if len(desc.Reservations) > 0 && len(desc.Reservations[0].Instances) > 0 {
			current := desc.Reservations[0].Instances[0].State.Name
			if current == target {
				return nil
			}
			if current == ec2types.InstanceStateNameTerminated {
				return fmt.Errorf("instance terminated unexpectedly")
			}
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(ec2PollInterval):
		}
	}
}

func (p *EC2Provider) createKeyPair(ctx context.Context) error {
	keyName := fmt.Sprintf("gitmachine-%d", time.Now().UnixNano())

	privKey, err := rsa.GenerateKey(rand.Reader, 4096)
	if err != nil {
		return fmt.Errorf("generate rsa key: %w", err)
	}

	privPEM := pem.EncodeToMemory(&pem.Block{
		Type:  "RSA PRIVATE KEY",
		Bytes: x509.MarshalPKCS1PrivateKey(privKey),
	})

	pubKey, err := ssh.NewPublicKey(&privKey.PublicKey)
	if err != nil {
		return fmt.Errorf("create ssh public key: %w", err)
	}

	_, err = p.ec2Client.ImportKeyPair(ctx, &ec2.ImportKeyPairInput{
		KeyName:           aws.String(keyName),
		PublicKeyMaterial: ssh.MarshalAuthorizedKey(pubKey),
	})
	if err != nil {
		return fmt.Errorf("import key pair: %w", err)
	}

	p.keyName = keyName
	p.privateKey = string(privPEM)
	p.createdKeyName = keyName
	return nil
}

const sharedSGName = "gitmachine-ssh"

func (p *EC2Provider) createSecurityGroup(ctx context.Context) error {
	// Try to find the shared "gitmachine-ssh" security group first.
	desc, err := p.ec2Client.DescribeSecurityGroups(ctx, &ec2.DescribeSecurityGroupsInput{
		Filters: []ec2types.Filter{{
			Name:   aws.String("group-name"),
			Values: []string{sharedSGName},
		}},
	})
	if err == nil && len(desc.SecurityGroups) > 0 {
		sgID := *desc.SecurityGroups[0].GroupId
		p.sgIDs = []string{sgID}
		// Not marked as createdSGID — shared SG is never auto-deleted.
		return nil
	}

	// Create the shared SG.
	result, err := p.ec2Client.CreateSecurityGroup(ctx, &ec2.CreateSecurityGroupInput{
		GroupName:   aws.String(sharedSGName),
		Description: aws.String("GitMachine shared SSH access"),
		TagSpecifications: []ec2types.TagSpecification{{
			ResourceType: ec2types.ResourceTypeSecurityGroup,
			Tags: []ec2types.Tag{{
				Key:   aws.String("managed-by"),
				Value: aws.String("gitmachine"),
			}},
		}},
	})
	if err != nil {
		return fmt.Errorf("create security group: %w", err)
	}

	sgID := *result.GroupId

	_, err = p.ec2Client.AuthorizeSecurityGroupIngress(ctx, &ec2.AuthorizeSecurityGroupIngressInput{
		GroupId: aws.String(sgID),
		IpPermissions: []ec2types.IpPermission{{
			IpProtocol: aws.String("tcp"),
			FromPort:   aws.Int32(22),
			ToPort:     aws.Int32(22),
			IpRanges:   []ec2types.IpRange{{CidrIp: aws.String("0.0.0.0/0")}},
		}},
	})
	if err != nil {
		return fmt.Errorf("authorize ssh ingress: %w", err)
	}

	p.sgIDs = []string{sgID}
	// Not marked as createdSGID — shared SG persists across nodes.
	return nil
}

// Compile-time check.
var _ CloudProvider = (*EC2Provider)(nil)
