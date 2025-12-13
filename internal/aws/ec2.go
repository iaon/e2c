// SPDX-FileCopyrightText: Copyright (C) Nicolas Lamirault <nicolas.lamirault@gmail.com>
// SPDX-License-Identifier: Apache-2.0

package aws

import (
	"context"
	"fmt"
	"log/slog"
	"sort"
	"sync"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/ec2"
	"github.com/aws/aws-sdk-go-v2/service/ec2/types"

	"github.com/nlamirault/e2c/internal/model"
)

// EC2Client handles interactions with AWS EC2 API
type EC2Client struct {
	client     *ec2.Client
	log        *slog.Logger
	region     string
	instancesM sync.Mutex
	instances  []model.Instance
}

// GetRegion returns the current AWS region
func (c *EC2Client) GetRegion() string {
	return c.region
}

// NewEC2Client creates a new EC2 client
func NewEC2Client(log *slog.Logger, region, profile string) (*EC2Client, error) {
	log.Info("Creating new EC2 client",
		"region", region,
		"profile", profile,
	)

	// Configure AWS SDK
	var cfg aws.Config
	var err error

	if profile != "" {
		log.Info("Loading AWS config with profile", "profile", profile)
		cfg, err = config.LoadDefaultConfig(
			context.Background(),
			config.WithRegion(region),
			config.WithSharedConfigProfile(profile),
		)
	} else {
		log.Info("Loading AWS config without profile", "region", region)
		cfg, err = config.LoadDefaultConfig(
			context.Background(),
			config.WithRegion(region),
		)
	}

	if err != nil {
		return nil, fmt.Errorf("failed to load AWS config: %w", err)
	}

	// Create EC2 client
	client := ec2.NewFromConfig(cfg)

	return &EC2Client{
		client: client,
		log:    log,
		region: region,
	}, nil
}

// ListInstances retrieves all EC2 instances in the region
func (c *EC2Client) ListInstances(ctx context.Context) ([]model.Instance, error) {
	c.log.Info("Listing EC2 instances")

	input := &ec2.DescribeInstancesInput{}
	result, err := c.client.DescribeInstances(ctx, input)
	if err != nil {
		return nil, fmt.Errorf("failed to describe instances: %w", err)
	}

	instances := make([]model.Instance, 0)
	protections := make(map[string]struct {
		termEnabled bool
		stopEnabled bool
	})

	var protectionsM sync.Mutex
	var wg sync.WaitGroup
	sem := make(chan struct{}, 5)

	for _, reservation := range result.Reservations {
		for _, instance := range reservation.Instances {
			instanceID := aws.ToString(instance.InstanceId)

			wg.Add(1)
			go func(id string) {
				defer wg.Done()
				sem <- struct{}{}
				termProtection, stopProtection, err := c.getProtectionAttributes(ctx, id)
				<-sem

				if err != nil {
					c.log.Warn("Failed to get instance protections", "instanceID", id, "error", err)
				}

				protectionsM.Lock()
				protections[id] = struct {
					termEnabled bool
					stopEnabled bool
				}{
					termEnabled: termProtection,
					stopEnabled: stopProtection,
				}
				protectionsM.Unlock()
			}(instanceID)
		}
	}

	wg.Wait()

	for _, reservation := range result.Reservations {
		for _, instance := range reservation.Instances {
			instanceID := aws.ToString(instance.InstanceId)
			protection := protections[instanceID]

			i := convertToModelInstance(instance, c.region, protection.termEnabled, protection.stopEnabled)
			instances = append(instances, i)
		}
	}

	// Sort instances by name or instance ID if name not available
	sort.Slice(instances, func(i, j int) bool {
		if instances[i].Name == "" && instances[j].Name == "" {
			return instances[i].ID < instances[j].ID
		}
		if instances[i].Name == "" {
			return false
		}
		if instances[j].Name == "" {
			return true
		}
		return instances[i].Name < instances[j].Name
	})

	c.instancesM.Lock()
	c.instances = instances
	c.instancesM.Unlock()

	c.log.Info("Retrieved EC2 instances", "count", len(instances))

	return instances, nil
}

// GetInstances returns the cached instances
func (c *EC2Client) GetInstances() []model.Instance {
	c.instancesM.Lock()
	defer c.instancesM.Unlock()
	return c.instances
}

// StartInstance starts an EC2 instance
func (c *EC2Client) StartInstance(ctx context.Context, instanceID string) error {
	c.log.Info("Starting EC2 instance", "instanceID", instanceID)

	input := &ec2.StartInstancesInput{
		InstanceIds: []string{instanceID},
	}

	_, err := c.client.StartInstances(ctx, input)
	if err != nil {
		return fmt.Errorf("failed to start instance %s: %w", instanceID, err)
	}

	return nil
}

// StopInstance stops an EC2 instance
func (c *EC2Client) StopInstance(ctx context.Context, instanceID string) error {
	c.log.Info("Stopping EC2 instance", "instanceID", instanceID)

	input := &ec2.StopInstancesInput{
		InstanceIds: []string{instanceID},
	}

	_, err := c.client.StopInstances(ctx, input)
	if err != nil {
		return fmt.Errorf("failed to stop instance %s: %w", instanceID, err)
	}

	return nil
}

// RebootInstance reboots an EC2 instance
func (c *EC2Client) RebootInstance(ctx context.Context, instanceID string) error {
	c.log.Info("Rebooting EC2 instance", "instanceID", instanceID)

	input := &ec2.RebootInstancesInput{
		InstanceIds: []string{instanceID},
	}

	_, err := c.client.RebootInstances(ctx, input)
	if err != nil {
		return fmt.Errorf("failed to reboot instance %s: %w", instanceID, err)
	}

	return nil
}

// TerminateInstance terminates an EC2 instance
func (c *EC2Client) TerminateInstance(ctx context.Context, instanceID string) error {
	c.log.Info("Terminating EC2 instance", "instanceID", instanceID)

	input := &ec2.TerminateInstancesInput{
		InstanceIds: []string{instanceID},
	}

	_, err := c.client.TerminateInstances(ctx, input)
	if err != nil {
		return fmt.Errorf("failed to terminate instance %s: %w", instanceID, err)
	}

	return nil
}

// GetInstanceConsoleOutput retrieves the console output of an EC2 instance
func (c *EC2Client) GetInstanceConsoleOutput(ctx context.Context, instanceID string) (string, error) {
	c.log.Info("Getting console output for EC2 instance", "instanceID", instanceID)

	input := &ec2.GetConsoleOutputInput{
		InstanceId: aws.String(instanceID),
	}

	output, err := c.client.GetConsoleOutput(ctx, input)
	if err != nil {
		return "", fmt.Errorf("failed to get console output for instance %s: %w", instanceID, err)
	}

	if output.Output == nil {
		return "No console output available", nil
	}

	return *output.Output, nil
}

// SetTerminationProtection enables or disables termination protection on an instance
func (c *EC2Client) SetTerminationProtection(ctx context.Context, instanceID string, enabled bool) error {
	c.log.Info("Updating termination protection", "instanceID", instanceID, "enabled", enabled)

	input := &ec2.ModifyInstanceAttributeInput{
		InstanceId:            aws.String(instanceID),
		DisableApiTermination: &types.AttributeBooleanValue{Value: aws.Bool(enabled)},
	}

	if _, err := c.client.ModifyInstanceAttribute(ctx, input); err != nil {
		return fmt.Errorf("failed to update termination protection for instance %s: %w", instanceID, err)
	}

	return nil
}

// SetStopProtection enables or disables stop protection on an instance
func (c *EC2Client) SetStopProtection(ctx context.Context, instanceID string, enabled bool) error {
	c.log.Info("Updating stop protection", "instanceID", instanceID, "enabled", enabled)

	input := &ec2.ModifyInstanceAttributeInput{
		InstanceId:     aws.String(instanceID),
		DisableApiStop: &types.AttributeBooleanValue{Value: aws.Bool(enabled)},
	}

	if _, err := c.client.ModifyInstanceAttribute(ctx, input); err != nil {
		return fmt.Errorf("failed to update stop protection for instance %s: %w", instanceID, err)
	}

	return nil
}

func (c *EC2Client) getProtectionAttributes(ctx context.Context, instanceID string) (bool, bool, error) {
	termAttr, err := c.client.DescribeInstanceAttribute(ctx, &ec2.DescribeInstanceAttributeInput{
		InstanceId: aws.String(instanceID),
		Attribute:  types.InstanceAttributeNameDisableApiTermination,
	})
	if err != nil {
		return false, false, fmt.Errorf("failed to describe termination protection: %w", err)
	}

	stopAttr, err := c.client.DescribeInstanceAttribute(ctx, &ec2.DescribeInstanceAttributeInput{
		InstanceId: aws.String(instanceID),
		Attribute:  types.InstanceAttributeNameDisableApiStop,
	})
	if err != nil {
		return false, false, fmt.Errorf("failed to describe stop protection: %w", err)
	}

	termEnabled := termAttr.DisableApiTermination != nil && termAttr.DisableApiTermination.Value != nil && aws.ToBool(termAttr.DisableApiTermination.Value)
	stopEnabled := stopAttr.DisableApiStop != nil && stopAttr.DisableApiStop.Value != nil && aws.ToBool(stopAttr.DisableApiStop.Value)

	return termEnabled, stopEnabled, nil
}

// convertToModelInstance converts an EC2 instance to our internal model
func convertToModelInstance(instance types.Instance, region string, terminationProtection, stopProtection bool) model.Instance {
	i := model.Instance{
		ID:                    *instance.InstanceId,
		Type:                  string(instance.InstanceType),
		State:                 string(instance.State.Name),
		Region:                region,
		LaunchTime:            aws.ToTime(instance.LaunchTime),
		PrivateIP:             aws.ToString(instance.PrivateIpAddress),
		PublicIP:              aws.ToString(instance.PublicIpAddress),
		Platform:              aws.ToString(instance.PlatformDetails),
		Architecture:          string(instance.Architecture),
		Tags:                  make(map[string]string),
		TerminationProtection: terminationProtection,
		StopProtection:        stopProtection,
	}

	// Extract all tags
	for _, tag := range instance.Tags {
		key := aws.ToString(tag.Key)
		value := aws.ToString(tag.Value)
		i.Tags[key] = value

		// Set name separately for backwards compatibility
		if key == "Name" {
			i.Name = value
		}
	}

	// Set age
	if !i.LaunchTime.IsZero() {
		i.Age = time.Since(i.LaunchTime).Round(time.Second)
	}

	return i
}
