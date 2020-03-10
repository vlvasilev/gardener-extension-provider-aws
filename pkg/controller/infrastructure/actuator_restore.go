// Copyright (c) 2019 SAP SE or an SAP affiliate company. All rights reserved. This file is licensed under the Apache Software License, v. 2 except as noted otherwise in the LICENSE file
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//      http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package infrastructure

import (
	"context"
	"fmt"
	"path/filepath"
	"time"

	awsapi "github.com/gardener/gardener-extension-provider-aws/pkg/apis/aws"
	"github.com/gardener/gardener-extension-provider-aws/pkg/aws"

	extensionscontroller "github.com/gardener/gardener-extensions/pkg/controller"
	controllererrors "github.com/gardener/gardener-extensions/pkg/controller/error"
	"github.com/gardener/gardener-extensions/pkg/terraformer"
	extensionsv1alpha1 "github.com/gardener/gardener/pkg/apis/extensions/v1alpha1"
)

func (a *actuator) Restore(ctx context.Context, infrastructure *extensionsv1alpha1.Infrastructure, cluster *extensionscontroller.Cluster) error {
	credentials, err := aws.GetCredentialsFromSecretRef(ctx, a.Client(), infrastructure.Spec.SecretRef)
	if err != nil {
		return err
	}

	infrastructureConfig := &awsapi.InfrastructureConfig{}
	if _, _, err := a.Decoder().Decode(infrastructure.Spec.ProviderConfig.Raw, nil, infrastructureConfig); err != nil {
		return fmt.Errorf("could not decode provider config: %+v", err)
	}

	terraformConfig, err := generateTerraformInfraConfig(ctx, infrastructure, infrastructureConfig, credentials)
	if err != nil {
		return fmt.Errorf("failed to generate Terraform config: %+v", err)
	}

	terraformState, err := terraformer.UnmarshalRawState(infrastructure.Status.State)
	if err != nil {
		return err
	}

	release, err := a.ChartRenderer().Render(filepath.Join(aws.InternalChartsPath, "aws-infra"), "aws-infra", infrastructure.Namespace, terraformConfig)
	if err != nil {
		return fmt.Errorf("could not render Terraform chart: %+v", err)
	}

	tf, err := a.newTerraformer(aws.TerraformerPurposeInfra, infrastructure.Namespace, infrastructure.Name)
	if err != nil {
		return fmt.Errorf("could not create terraformer object: %+v", err)
	}

	if err := tf.
		SetVariablesEnvironment(generateTerraformInfraVariablesEnvironment(credentials)).
		InitializeWith(terraformer.DefaultInitializer(
			a.Client(),
			release.FileContent("main.tf"),
			release.FileContent("variables.tf"),
			[]byte(release.FileContent("terraform.tfvars")),
			terraformState.Data,
		)).
		Apply(); err != nil {

		a.logger.Error(err, "failed to apply the terraform config", "infrastructure", infrastructure.Name)
		return &controllererrors.RequeueAfterError{
			Cause:        err,
			RequeueAfter: 30 * time.Second,
		}
	}

	return a.updateProviderStatus(ctx, tf, infrastructure, infrastructureConfig)
}
