/*
Copyright 2016 The Kubernetes Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package model

import (
	"fmt"
	"github.com/golang/glog"
	"k8s.io/kops/pkg/apis/kops"
	"k8s.io/kops/pkg/model/resources"
	"k8s.io/kops/upup/pkg/fi"
	"k8s.io/kops/upup/pkg/fi/cloudup/awstasks"
	"k8s.io/kops/upup/pkg/fi/nodeup"
	"text/template"
)

const (
	DefaultVolumeSize = 20
	DefaultVolumeType = "gp2"
)

// AutoscalingGroupModelBuilder configures AutoscalingGroup objects
type AutoscalingGroupModelBuilder struct {
	*KopsModelContext

	NodeUpSource     string
	NodeUpSourceHash string

	NodeUpConfigBuilder func(ig *kops.InstanceGroup) (*nodeup.NodeUpConfig, error)
}

var _ fi.ModelBuilder = &AutoscalingGroupModelBuilder{}

func (b *AutoscalingGroupModelBuilder) Build(c *fi.ModelBuilderContext) error {
	for _, ig := range b.InstanceGroups {
		name := b.AutoscalingGroupName(ig)

		// LaunchConfiguration
		var launchConfiguration *awstasks.LaunchConfiguration
		{
			volumeSize := fi.Int32Value(ig.Spec.RootVolumeSize)
			if volumeSize == 0 {
				volumeSize = DefaultVolumeSize
			}
			volumeType := fi.StringValue(ig.Spec.RootVolumeType)
			if volumeType == "" {
				volumeType = DefaultVolumeType
			}

			t := &awstasks.LaunchConfiguration{
				Name: s(name),

				SecurityGroups: []*awstasks.SecurityGroup{
					b.LinkToSecurityGroup(ig.Spec.Role),
				},
				IAMInstanceProfile: b.LinkToIAMInstanceProfile(ig),
				ImageID:            s(ig.Spec.Image),
				InstanceType:       s(ig.Spec.MachineType),

				RootVolumeSize: i64(int64(volumeSize)),
				RootVolumeType: s(volumeType),
			}

			var err error

			if t.SSHKey, err = b.LinkToSSHKey(); err != nil {
				return err
			}

			if t.UserData, err = b.resourceNodeUp(ig); err != nil {
				return err
			}

			if fi.StringValue(ig.Spec.MaxPrice) != "" {
				spotPrice := fi.StringValue(ig.Spec.MaxPrice)
				t.SpotPrice = spotPrice
			}

			{
				// TODO: Wrapper / helper class to analyze clusters
				subnetMap := make(map[string]*kops.ClusterSubnetSpec)
				for i := range b.Cluster.Spec.Subnets {
					subnet := &b.Cluster.Spec.Subnets[i]
					subnetMap[subnet.Name] = subnet
				}

				var subnetType kops.SubnetType
				for _, subnetName := range ig.Spec.Subnets {
					subnet := subnetMap[subnetName]
					if subnet == nil {
						return fmt.Errorf("InstanceGroup %q uses subnet %q that does not exist", ig.ObjectMeta.Name, subnetName)
					}
					if subnetType != "" && subnetType != subnet.Type {
						return fmt.Errorf("InstanceGroup %q cannot be in subnets of different Type", ig.ObjectMeta.Name)
					}
					subnetType = subnet.Type
				}

				associatePublicIP := true
				switch subnetType {
				case kops.SubnetTypePublic, kops.SubnetTypeUtility:
					associatePublicIP = true
					if ig.Spec.AssociatePublicIP != nil {
						associatePublicIP = *ig.Spec.AssociatePublicIP
					}

				case kops.SubnetTypePrivate:
					associatePublicIP = false
					if ig.Spec.AssociatePublicIP != nil {
						// This isn't meaningful - private subnets can't have public ip
						//associatePublicIP = *ig.Spec.AssociatePublicIP
						if *ig.Spec.AssociatePublicIP {
							glog.Warningf("Ignoring private InstanceGroup %q with AssociatePublicIP=true", ig.ObjectMeta.Name)
						}
					}

				default:
					return fmt.Errorf("unknown subnet type %q", subnetType)
				}
				t.AssociatePublicIP = &associatePublicIP
			}
			c.AddTask(t)

			launchConfiguration = t
		}

		// AutoscalingGroup
		{
			t := &awstasks.AutoscalingGroup{
				Name: s(name),

				LaunchConfiguration: launchConfiguration,
			}

			minSize := int32(1)
			maxSize := int32(1)
			if ig.Spec.MinSize != nil {
				minSize = fi.Int32Value(ig.Spec.MinSize)
			} else if ig.Spec.Role == kops.InstanceGroupRoleNode {
				minSize = 2
			}
			if ig.Spec.MaxSize != nil {
				maxSize = *ig.Spec.MaxSize
			} else if ig.Spec.Role == kops.InstanceGroupRoleNode {
				maxSize = 2
			}

			t.MinSize = i64(int64(minSize))
			t.MaxSize = i64(int64(maxSize))

			subnets, err := b.GatherSubnets(ig)
			if err != nil {
				return err
			}
			if len(subnets) == 0 {
				return fmt.Errorf("could not determine any subnets for InstanceGroup %q; subnets was %s", ig.ObjectMeta.Name, ig.Spec.Subnets)
			}
			for _, subnet := range subnets {
				t.Subnets = append(t.Subnets, b.LinkToSubnet(subnet))
			}

			tags, err := b.CloudTagsForInstanceGroup(ig)
			if err != nil {
				return fmt.Errorf("error building cloud tags: %v", err)
			}
			t.Tags = tags

			c.AddTask(t)
		}
	}

	return nil
}

func (b *AutoscalingGroupModelBuilder) resourceNodeUp(ig *kops.InstanceGroup) (*fi.ResourceHolder, error) {
	if ig.Spec.Role == kops.InstanceGroupRoleBastion {
		// Bastions are just bare machines (currently), used as SSH jump-hosts
		return nil, nil
	}

	functions := template.FuncMap{
		"NodeUpSource": func() string {
			return b.NodeUpSource
		},
		"NodeUpSourceHash": func() string {
			return b.NodeUpSourceHash
		},
		"KubeEnv": func() (string, error) {
			config, err := b.NodeUpConfigBuilder(ig)
			if err != nil {
				return "", err
			}

			data, err := kops.ToRawYaml(config)
			if err != nil {
				return "", err
			}

			return string(data), nil
		},
	}

	templateResource, err := NewTemplateResource("nodeup", resources.AWSNodeUpTemplate, functions, nil)
	if err != nil {
		return nil, err
	}
	return fi.WrapResource(templateResource), nil
}
