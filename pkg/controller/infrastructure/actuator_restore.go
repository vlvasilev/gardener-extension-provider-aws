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
	awsv1alpha1 "github.com/gardener/gardener-extension-provider-aws/pkg/apis/aws/v1alpha1"
	"github.com/gardener/gardener-extension-provider-aws/pkg/aws"

	extensionscontroller "github.com/gardener/gardener-extensions/pkg/controller"
	controllererrors "github.com/gardener/gardener-extensions/pkg/controller/error"
	"github.com/gardener/gardener-extensions/pkg/terraformer"
	extensionsv1alpha1 "github.com/gardener/gardener/pkg/apis/extensions/v1alpha1"
	"github.com/gardener/gardener/pkg/chartrenderer"
	"github.com/go-logr/logr"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/rest"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

func (a *actuator) Restore(ctx context.Context, infrastructure *extensionsv1alpha1.Infrastructure, _ *extensionscontroller.Cluster) error {
	infrastructureStatus, state, err := Restore(ctx, a.logger, a.RESTConfig(), a.Client(), a.Decoder(), a.ChartRenderer(), infrastructure)
	if err != nil {
		return err
	}
	return updateProviderStatus(ctx, a.Client(), infrastructure, infrastructureStatus, state)
}

// Restore restores the given Infrastructure object. It returns the provider specific status and the Terraform state.
func Restore(
	ctx context.Context,
	logger logr.Logger,
	restConfig *rest.Config,
	c client.Client,
	decoder runtime.Decoder,
	chartRenderer chartrenderer.Interface,
	infrastructure *extensionsv1alpha1.Infrastructure,
) (
	*awsv1alpha1.InfrastructureStatus,
	*terraformer.RawState,
	error,
) {
	credentials, err := aws.GetCredentialsFromSecretRef(ctx, c, infrastructure.Spec.SecretRef)
	if err != nil {
		return nil, nil, err
	}

	infrastructureConfig := &awsapi.InfrastructureConfig{}
	if _, _, err := decoder.Decode(infrastructure.Spec.ProviderConfig.Raw, nil, infrastructureConfig); err != nil {
		return nil, nil, fmt.Errorf("could not decode provider config: %+v", err)
	}

	terraformConfig, err := generateTerraformInfraConfig(ctx, infrastructure, infrastructureConfig, credentials)
	if err != nil {
		return nil, nil, fmt.Errorf("failed to generate Terraform config: %+v", err)
	}

	terraformState, err := terraformer.UnmarshalRawState(infrastructure.Status.State)
	if err != nil {
		return nil, nil, err
	}

	release, err := chartRenderer.Render(filepath.Join(aws.InternalChartsPath, "aws-infra"), "aws-infra", infrastructure.Namespace, terraformConfig)
	if err != nil {
		return nil, nil, fmt.Errorf("could not render Terraform chart: %+v", err)
	}

	tf, err := newTerraformer(restConfig, aws.TerraformerPurposeInfra, infrastructure.Namespace, infrastructure.Name)
	if err != nil {
		return nil, nil, fmt.Errorf("could not create terraformer object: %+v", err)
	}

	if err := tf.
		SetVariablesEnvironment(generateTerraformInfraVariablesEnvironment(credentials)).
		InitializeWith(terraformer.StateInitializer(
			c,
			release.FileContent("main.tf"),
			release.FileContent("variables.tf"),
			[]byte(release.FileContent("terraform.tfvars")),
			terraformState.Data,
		)).
		Apply(); err != nil {

		logger.Error(err, "failed to apply the terraform config", "infrastructure", infrastructure.Name)
		return nil, nil, &controllererrors.RequeueAfterError{
			Cause:        err,
			RequeueAfter: 30 * time.Second,
		}
	}

	return computeProviderStatus(ctx, tf, infrastructureConfig)
}
