// Copyright Amazon.com, Inc. or its affiliates. All Rights Reserved.
// SPDX-License-Identifier: Apache-2.0

package stack

import (
	"bytes"
	"errors"
	"fmt"
	"testing"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/cloudformation"
	"github.com/aws/copilot-cli/internal/pkg/addon"
	"github.com/aws/copilot-cli/internal/pkg/deploy"
	"github.com/aws/copilot-cli/internal/pkg/deploy/cloudformation/stack/mocks"
	"github.com/aws/copilot-cli/internal/pkg/manifest"
	"github.com/aws/copilot-cli/internal/pkg/template"
	"github.com/aws/copilot-cli/internal/pkg/template/override"
	"github.com/golang/mock/gomock"
	"github.com/stretchr/testify/require"
)

const (
	testEnvName      = "test"
	testAppName      = "phonetool"
	testImageRepoURL = "111111111111.dkr.ecr.us-west-2.amazonaws.com/phonetool/frontend"
	testImageTag     = "manual-bf3678c"
)

type mockAddons struct {
	tpl    string
	tplErr error

	params    string
	paramsErr error
}

func (m mockAddons) Template() (string, error) {
	if m.tplErr != nil {
		return "", m.tplErr
	}
	return m.tpl, nil
}

func (m mockAddons) Parameters() (string, error) {
	if m.paramsErr != nil {
		return "", m.paramsErr
	}
	return m.params, nil
}

var mockCloudFormationOverrideFunc = func(overrideRules []override.Rule, origTemp []byte) ([]byte, error) {
	return origTemp, nil
}

func TestLoadBalancedWebService_StackName(t *testing.T) {
	testCases := map[string]struct {
		inSvcName string
		inEnvName string
		inAppName string

		wantedStackName string
	}{
		"valid stack name": {
			inSvcName: "frontend",
			inEnvName: "test",
			inAppName: "phonetool",

			wantedStackName: "phonetool-test-frontend",
		},
		"longer than 128 characters": {
			inSvcName: "whatisthishorriblylongservicenamethatcantfitintocloudformationwhatarewesupposedtodoaboutthisaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
			inEnvName: "test",
			inAppName: "phonetool",

			wantedStackName: "phonetool-test-whatisthishorriblylongservicenamethatcantfitintocloudformationwhatarewesupposedtodoaboutthisaaaaaaaaaaaaaaaaaaaaa",
		},
	}

	for name, tc := range testCases {
		t.Run(name, func(t *testing.T) {
			// GIVEN
			conf := &LoadBalancedWebService{
				ecsWkld: &ecsWkld{
					wkld: &wkld{
						name: tc.inSvcName,
						env:  tc.inEnvName,
						app:  tc.inAppName,
					},
				},
			}

			// WHEN
			n := conf.StackName()

			// THEN
			require.Equal(t, tc.wantedStackName, n, "expected stack names to be equal")
		})
	}
}

func TestLoadBalancedWebService_Template(t *testing.T) {
	var overridenContainerHealthCheck = template.ContainerHealthCheck{
		Command:     []string{"CMD-SHELL", "curl -f http://localhost/ || exit 1"},
		Interval:    aws.Int64(10),
		StartPeriod: aws.Int64(0),
		Timeout:     aws.Int64(5),
		Retries:     aws.Int64(5),
	}
	var testLBWebServiceManifest = manifest.NewLoadBalancedWebService(&manifest.LoadBalancedWebServiceProps{
		WorkloadProps: &manifest.WorkloadProps{
			Name:       "frontend",
			Dockerfile: "frontend/Dockerfile",
		},
		Path: "frontend",
		Port: 80,
	})
	testLBWebServiceManifest.ImageConfig.HealthCheck = manifest.ContainerHealthCheck{
		Retries: aws.Int(5),
	}
	testLBWebServiceManifest.Alias = manifest.Alias{String: aws.String("mockAlias")}
	testLBWebServiceManifest.EntryPoint = manifest.EntryPointOverride{
		String:      nil,
		StringSlice: []string{"/bin/echo", "hello"},
	}
	testLBWebServiceManifest.Command = manifest.CommandOverride{
		String:      nil,
		StringSlice: []string{"world"},
	}

	testCases := map[string]struct {
		mockDependencies func(t *testing.T, ctrl *gomock.Controller, c *LoadBalancedWebService)
		wantedTemplate   string
		wantedError      error
	}{
		"unavailable rule priority lambda template": {
			mockDependencies: func(t *testing.T, ctrl *gomock.Controller, c *LoadBalancedWebService) {
				m := mocks.NewMockloadBalancedWebSvcReadParser(ctrl)
				m.EXPECT().Read(lbWebSvcRulePriorityGeneratorPath).Return(nil, errors.New("some error"))
				c.parser = m
			},
			wantedTemplate: "",
			wantedError:    fmt.Errorf("read rule priority lambda: some error"),
		},
		"unavailable desired count lambda template": {
			mockDependencies: func(t *testing.T, ctrl *gomock.Controller, c *LoadBalancedWebService) {
				m := mocks.NewMockloadBalancedWebSvcReadParser(ctrl)
				m.EXPECT().Read(lbWebSvcRulePriorityGeneratorPath).Return(&template.Content{Buffer: bytes.NewBufferString("something")}, nil)
				m.EXPECT().Read(desiredCountGeneratorPath).Return(nil, errors.New("some error"))
				c.parser = m
			},
			wantedTemplate: "",
			wantedError:    fmt.Errorf("read desired count lambda: some error"),
		},
		"unavailable env controller lambda template": {
			mockDependencies: func(t *testing.T, ctrl *gomock.Controller, c *LoadBalancedWebService) {
				m := mocks.NewMockloadBalancedWebSvcReadParser(ctrl)
				m.EXPECT().Read(lbWebSvcRulePriorityGeneratorPath).Return(&template.Content{Buffer: bytes.NewBufferString("something")}, nil)
				m.EXPECT().Read(desiredCountGeneratorPath).Return(&template.Content{Buffer: bytes.NewBufferString("something")}, nil)
				m.EXPECT().Read(envControllerPath).Return(nil, errors.New("some error"))
				c.parser = m
			},
			wantedTemplate: "",
			wantedError:    fmt.Errorf("read env controller lambda: some error"),
		},
		"unexpected addons template parsing error": {
			mockDependencies: func(t *testing.T, ctrl *gomock.Controller, c *LoadBalancedWebService) {
				m := mocks.NewMockloadBalancedWebSvcReadParser(ctrl)
				m.EXPECT().Read(lbWebSvcRulePriorityGeneratorPath).Return(&template.Content{Buffer: bytes.NewBufferString("something")}, nil)
				m.EXPECT().Read(desiredCountGeneratorPath).Return(&template.Content{Buffer: bytes.NewBufferString("something")}, nil)
				m.EXPECT().Read(envControllerPath).Return(&template.Content{Buffer: bytes.NewBufferString("something")}, nil)
				addons := mockAddons{tplErr: errors.New("some error")}
				c.parser = m
				c.wkld.addons = addons
			},
			wantedError: fmt.Errorf("generate addons template for %s: %w", aws.StringValue(testLBWebServiceManifest.Name), errors.New("some error")),
		},
		"unexpected addons parameter parsing error": {
			mockDependencies: func(t *testing.T, ctrl *gomock.Controller, c *LoadBalancedWebService) {
				m := mocks.NewMockloadBalancedWebSvcReadParser(ctrl)
				m.EXPECT().Read(lbWebSvcRulePriorityGeneratorPath).Return(&template.Content{Buffer: bytes.NewBufferString("something")}, nil)
				m.EXPECT().Read(desiredCountGeneratorPath).Return(&template.Content{Buffer: bytes.NewBufferString("something")}, nil)
				m.EXPECT().Read(envControllerPath).Return(&template.Content{Buffer: bytes.NewBufferString("something")}, nil)
				addons := mockAddons{paramsErr: errors.New("some error")}
				c.parser = m
				c.wkld.addons = addons
			},
			wantedError: fmt.Errorf("parse addons parameters for %s: %w", aws.StringValue(testLBWebServiceManifest.Name), errors.New("some error")),
		},
		"failed parsing svc template": {
			mockDependencies: func(t *testing.T, ctrl *gomock.Controller, c *LoadBalancedWebService) {
				m := mocks.NewMockloadBalancedWebSvcReadParser(ctrl)
				m.EXPECT().Read(lbWebSvcRulePriorityGeneratorPath).Return(&template.Content{Buffer: bytes.NewBufferString("something")}, nil)
				m.EXPECT().Read(desiredCountGeneratorPath).Return(&template.Content{Buffer: bytes.NewBufferString("something")}, nil)
				m.EXPECT().Read(envControllerPath).Return(&template.Content{Buffer: bytes.NewBufferString("something")}, nil)
				m.EXPECT().ParseLoadBalancedWebService(gomock.Any()).Return(nil, errors.New("some error"))
				addons := mockAddons{
					tpl: `
Resources:
  AdditionalResourcesPolicy:
    Type: AWS::IAM::ManagedPolicy
Outputs:
  AdditionalResourcesPolicyArn:
    Value: hello`,
				}
				c.parser = m
				c.wkld.addons = addons
			},

			wantedTemplate: "",
			wantedError:    fmt.Errorf("some error"),
		},
		"render template without addons": {
			mockDependencies: func(t *testing.T, ctrl *gomock.Controller, c *LoadBalancedWebService) {
				m := mocks.NewMockloadBalancedWebSvcReadParser(ctrl)
				m.EXPECT().Read(lbWebSvcRulePriorityGeneratorPath).Return(&template.Content{Buffer: bytes.NewBufferString("lambda")}, nil)
				m.EXPECT().Read(desiredCountGeneratorPath).Return(&template.Content{Buffer: bytes.NewBufferString("something")}, nil)
				m.EXPECT().Read(envControllerPath).Return(&template.Content{Buffer: bytes.NewBufferString("something")}, nil)
				m.EXPECT().ParseLoadBalancedWebService(template.WorkloadOpts{
					WorkloadType: manifest.LoadBalancedWebServiceType,
					HTTPHealthCheck: template.HTTPHealthCheckOpts{
						HealthCheckPath: "/",
						GracePeriod:     aws.Int64(60),
					},
					DeregistrationDelay: aws.Int64(60),
					HealthCheck:         &overridenContainerHealthCheck,
					RulePriorityLambda:  "lambda",
					DesiredCountLambda:  "something",
					EnvControllerLambda: "something",
					Network: template.NetworkOpts{
						AssignPublicIP: template.EnablePublicIP,
						SubnetsType:    template.PublicSubnetsPlacement,
					},
					EntryPoint: []string{"/bin/echo", "hello"},
					Command:    []string{"world"},
				}).Return(&template.Content{Buffer: bytes.NewBufferString("template")}, nil)

				addons := mockAddons{tplErr: &addon.ErrAddonsNotFound{}, paramsErr: &addon.ErrAddonsNotFound{}}
				c.parser = m
				c.wkld.addons = addons
			},

			wantedTemplate: "template",
		},
		"render template with addons": {
			mockDependencies: func(t *testing.T, ctrl *gomock.Controller, c *LoadBalancedWebService) {
				m := mocks.NewMockloadBalancedWebSvcReadParser(ctrl)
				m.EXPECT().Read(lbWebSvcRulePriorityGeneratorPath).Return(&template.Content{Buffer: bytes.NewBufferString("lambda")}, nil)
				m.EXPECT().Read(desiredCountGeneratorPath).Return(&template.Content{Buffer: bytes.NewBufferString("something")}, nil)
				m.EXPECT().Read(envControllerPath).Return(&template.Content{Buffer: bytes.NewBufferString("something")}, nil)
				m.EXPECT().ParseLoadBalancedWebService(template.WorkloadOpts{
					NestedStack: &template.WorkloadNestedStackOpts{
						StackName:       addon.StackName,
						VariableOutputs: []string{"Hello"},
						SecretOutputs:   []string{"MySecretArn"},
						PolicyOutputs:   []string{"AdditionalResourcesPolicyArn"},
					},
					AddonsExtraParams: "ServiceName: !GetAtt Service.Name",
					WorkloadType:      manifest.LoadBalancedWebServiceType,
					HTTPHealthCheck: template.HTTPHealthCheckOpts{
						HealthCheckPath: "/",
						GracePeriod:     aws.Int64(60),
					},
					DeregistrationDelay: aws.Int64(60),
					HealthCheck:         &overridenContainerHealthCheck,
					RulePriorityLambda:  "lambda",
					DesiredCountLambda:  "something",
					EnvControllerLambda: "something",
					Network: template.NetworkOpts{
						AssignPublicIP: template.EnablePublicIP,
						SubnetsType:    template.PublicSubnetsPlacement,
					},
					EntryPoint: []string{"/bin/echo", "hello"},
					Command:    []string{"world"},
				}).Return(&template.Content{Buffer: bytes.NewBufferString("template")}, nil)
				addons := mockAddons{
					tpl: `Resources:
  AdditionalResourcesPolicy:
    Type: AWS::IAM::ManagedPolicy
    Properties:
      PolicyDocument:
        Statement:
        - Effect: Allow
          Action: '*'
          Resource: '*'
  MySecret:
    Type: AWS::SecretsManager::Secret
    Properties:
      Description: 'This is my rds instance secret'
      GenerateSecretString:
        SecretStringTemplate: '{"username": "admin"}'
        GenerateStringKey: 'password'
        PasswordLength: 16
        ExcludeCharacters: '"@/\'
Outputs:
  AdditionalResourcesPolicyArn:
    Value: !Ref AdditionalResourcesPolicy
  MySecretArn:
    Value: !Ref MySecret
  Hello:
    Value: hello`,
					params: "ServiceName: !GetAtt Service.Name",
				}
				c.parser = m
				c.addons = addons
			},
			wantedTemplate: "template",
		},
	}

	for name, tc := range testCases {
		t.Run(name, func(t *testing.T) {
			// GIVEN
			ctrl := gomock.NewController(t)
			defer ctrl.Finish()
			conf := &LoadBalancedWebService{
				ecsWkld: &ecsWkld{
					wkld: &wkld{
						name: aws.StringValue(testLBWebServiceManifest.Name),
						env:  testEnvName,
						app:  testAppName,
						rc: RuntimeConfig{
							Image: &ECRImage{
								RepoURL:  testImageRepoURL,
								ImageTag: testImageTag,
							},
							AccountID: "0123456789012",
							Region:    "us-west-2",
						},
					},
					taskDefOverrideFunc: mockCloudFormationOverrideFunc,
				},
				manifest: testLBWebServiceManifest,
			}
			tc.mockDependencies(t, ctrl, conf)

			// WHEN
			template, err := conf.Template()

			// THEN
			if tc.wantedError != nil {
				require.EqualError(t, err, tc.wantedError.Error())
			} else {
				require.NoError(t, err)
				require.Equal(t, tc.wantedTemplate, template)
			}
		})
	}
}

func TestLoadBalancedWebService_Parameters(t *testing.T) {
	baseProps := &manifest.LoadBalancedWebServiceProps{
		WorkloadProps: &manifest.WorkloadProps{
			Name:       "frontend",
			Dockerfile: "frontend/Dockerfile",
		},
		Path: "frontend",
		Port: 80,
	}
	testLBWebServiceManifest := manifest.NewLoadBalancedWebService(baseProps)
	testLBWebServiceManifestRange := manifest.IntRangeBand("2-100")
	testLBWebServiceManifest.Count = manifest.Count{
		Value: aws.Int(1),
		AdvancedCount: manifest.AdvancedCount{
			Range: manifest.Range{
				Value: &testLBWebServiceManifestRange,
			},
		},
	}
	testLBWebServiceManifestWithBadCount := manifest.NewLoadBalancedWebService(baseProps)
	testLBWebServiceManifestWithBadCountRange := manifest.IntRangeBand("badCount")
	testLBWebServiceManifestWithBadCount.Count = manifest.Count{
		AdvancedCount: manifest.AdvancedCount{
			Range: manifest.Range{
				Value: &testLBWebServiceManifestWithBadCountRange,
			},
		},
	}
	testLBWebServiceManifestWithSidecar := manifest.NewLoadBalancedWebService(baseProps)
	testLBWebServiceManifestWithSidecar.TargetContainer = aws.String("xray")
	testLBWebServiceManifestWithSidecar.Sidecars = map[string]*manifest.SidecarConfig{
		"xray": {
			Port: aws.String("5000"),
		},
	}
	testLBWebServiceManifestWithStickiness := manifest.NewLoadBalancedWebService(baseProps)
	testLBWebServiceManifestWithStickiness.Stickiness = aws.Bool(true)
	testLBWebServiceManifestWithExecEnabled := manifest.NewLoadBalancedWebService(baseProps)
	testLBWebServiceManifestWithExecEnabled.ExecuteCommand = manifest.ExecuteCommand{
		Enable: aws.Bool(false),
		Config: manifest.ExecuteCommandConfig{
			Enable: aws.Bool(true),
		},
	}
	expectedParams := []*cloudformation.Parameter{
		{
			ParameterKey:   aws.String(WorkloadAppNameParamKey),
			ParameterValue: aws.String("phonetool"),
		},
		{
			ParameterKey:   aws.String(WorkloadEnvNameParamKey),
			ParameterValue: aws.String("test"),
		},
		{
			ParameterKey:   aws.String(WorkloadNameParamKey),
			ParameterValue: aws.String("frontend"),
		},
		{
			ParameterKey:   aws.String(WorkloadContainerImageParamKey),
			ParameterValue: aws.String("111111111111.dkr.ecr.us-west-2.amazonaws.com/phonetool/frontend:manual-bf3678c"),
		},
		{
			ParameterKey:   aws.String(LBWebServiceContainerPortParamKey),
			ParameterValue: aws.String("80"),
		},
		{
			ParameterKey:   aws.String(LBWebServiceRulePathParamKey),
			ParameterValue: aws.String("frontend"),
		},
		{
			ParameterKey:   aws.String(WorkloadTaskCPUParamKey),
			ParameterValue: aws.String("256"),
		},
		{
			ParameterKey:   aws.String(WorkloadTaskMemoryParamKey),
			ParameterValue: aws.String("512"),
		},
		{
			ParameterKey:   aws.String(WorkloadLogRetentionParamKey),
			ParameterValue: aws.String("30"),
		},
		{
			ParameterKey:   aws.String(WorkloadAddonsTemplateURLParamKey),
			ParameterValue: aws.String(""),
		},
		{
			ParameterKey:   aws.String(WorkloadEnvFileARNParamKey),
			ParameterValue: aws.String(""),
		},
	}
	testCases := map[string]struct {
		httpsEnabled         bool
		dnsDelegationEnabled bool
		manifest             *manifest.LoadBalancedWebService

		expectedParams []*cloudformation.Parameter
		expectedErr    error
	}{
		"HTTPS Enabled": {
			httpsEnabled: true,
			manifest:     testLBWebServiceManifest,

			expectedParams: append(expectedParams, []*cloudformation.Parameter{
				{
					ParameterKey:   aws.String(LBWebServiceHTTPSParamKey),
					ParameterValue: aws.String("true"),
				},
				{
					ParameterKey:   aws.String(LBWebServiceTargetContainerParamKey),
					ParameterValue: aws.String("frontend"),
				},
				{
					ParameterKey:   aws.String(LBWebServiceTargetPortParamKey),
					ParameterValue: aws.String("80"),
				},
				{
					ParameterKey:   aws.String(WorkloadTaskCountParamKey),
					ParameterValue: aws.String("2"),
				},
				{
					ParameterKey:   aws.String(LBWebServiceStickinessParamKey),
					ParameterValue: aws.String("false"),
				},
				{
					ParameterKey:   aws.String(LBWebServiceDNSDelegatedParamKey),
					ParameterValue: aws.String("true"),
				},
			}...),
		},
		"HTTPS Not Enabled": {
			httpsEnabled: false,
			manifest:     testLBWebServiceManifest,

			expectedParams: append(expectedParams, []*cloudformation.Parameter{
				{
					ParameterKey:   aws.String(LBWebServiceHTTPSParamKey),
					ParameterValue: aws.String("false"),
				},
				{
					ParameterKey:   aws.String(LBWebServiceTargetContainerParamKey),
					ParameterValue: aws.String("frontend"),
				},
				{
					ParameterKey:   aws.String(LBWebServiceTargetPortParamKey),
					ParameterValue: aws.String("80"),
				},
				{
					ParameterKey:   aws.String(WorkloadTaskCountParamKey),
					ParameterValue: aws.String("2"),
				},
				{
					ParameterKey:   aws.String(LBWebServiceStickinessParamKey),
					ParameterValue: aws.String("false"),
				},
				{
					ParameterKey:   aws.String(LBWebServiceDNSDelegatedParamKey),
					ParameterValue: aws.String("false"),
				},
			}...),
		},
		"with sidecar container": {
			httpsEnabled: true,
			manifest:     testLBWebServiceManifestWithSidecar,

			expectedParams: append(expectedParams, []*cloudformation.Parameter{
				{
					ParameterKey:   aws.String(LBWebServiceHTTPSParamKey),
					ParameterValue: aws.String("true"),
				},
				{
					ParameterKey:   aws.String(LBWebServiceTargetContainerParamKey),
					ParameterValue: aws.String("xray"),
				},
				{
					ParameterKey:   aws.String(LBWebServiceTargetPortParamKey),
					ParameterValue: aws.String("5000"),
				},
				{
					ParameterKey:   aws.String(WorkloadTaskCountParamKey),
					ParameterValue: aws.String("1"),
				},
				{
					ParameterKey:   aws.String(LBWebServiceStickinessParamKey),
					ParameterValue: aws.String("false"),
				},
				{
					ParameterKey:   aws.String(LBWebServiceDNSDelegatedParamKey),
					ParameterValue: aws.String("true"),
				},
			}...),
		},
		"Stickiness enabled": {
			httpsEnabled: false,
			manifest:     testLBWebServiceManifestWithStickiness,

			expectedParams: append(expectedParams, []*cloudformation.Parameter{
				{
					ParameterKey:   aws.String(LBWebServiceHTTPSParamKey),
					ParameterValue: aws.String("false"),
				},
				{
					ParameterKey:   aws.String(LBWebServiceTargetContainerParamKey),
					ParameterValue: aws.String("frontend"),
				},
				{
					ParameterKey:   aws.String(LBWebServiceTargetPortParamKey),
					ParameterValue: aws.String("80"),
				},
				{
					ParameterKey:   aws.String(WorkloadTaskCountParamKey),
					ParameterValue: aws.String("1"),
				},
				{
					ParameterKey:   aws.String(LBWebServiceStickinessParamKey),
					ParameterValue: aws.String("true"),
				},
				{
					ParameterKey:   aws.String(LBWebServiceDNSDelegatedParamKey),
					ParameterValue: aws.String("false"),
				},
			}...),
		},
		"exec enabled": {
			httpsEnabled: false,
			manifest:     testLBWebServiceManifestWithExecEnabled,

			expectedParams: append(expectedParams, []*cloudformation.Parameter{
				{
					ParameterKey:   aws.String(LBWebServiceHTTPSParamKey),
					ParameterValue: aws.String("false"),
				},
				{
					ParameterKey:   aws.String(LBWebServiceTargetContainerParamKey),
					ParameterValue: aws.String("frontend"),
				},
				{
					ParameterKey:   aws.String(LBWebServiceTargetPortParamKey),
					ParameterValue: aws.String("80"),
				},
				{
					ParameterKey:   aws.String(WorkloadTaskCountParamKey),
					ParameterValue: aws.String("1"),
				},
				{
					ParameterKey:   aws.String(LBWebServiceStickinessParamKey),
					ParameterValue: aws.String("false"),
				},
				{
					ParameterKey:   aws.String(LBWebServiceDNSDelegatedParamKey),
					ParameterValue: aws.String("false"),
				},
			}...),
		},
		"dns delegation enabled": {
			httpsEnabled:         false,
			dnsDelegationEnabled: true,

			manifest: testLBWebServiceManifestWithExecEnabled,

			expectedParams: append(expectedParams, []*cloudformation.Parameter{
				{
					ParameterKey:   aws.String(LBWebServiceHTTPSParamKey),
					ParameterValue: aws.String("false"),
				},
				{
					ParameterKey:   aws.String(LBWebServiceTargetContainerParamKey),
					ParameterValue: aws.String("frontend"),
				},
				{
					ParameterKey:   aws.String(LBWebServiceTargetPortParamKey),
					ParameterValue: aws.String("80"),
				},
				{
					ParameterKey:   aws.String(WorkloadTaskCountParamKey),
					ParameterValue: aws.String("1"),
				},
				{
					ParameterKey:   aws.String(LBWebServiceStickinessParamKey),
					ParameterValue: aws.String("false"),
				},
				{
					ParameterKey:   aws.String(LBWebServiceDNSDelegatedParamKey),
					ParameterValue: aws.String("true"),
				},
			}...),
		},
		"with bad count": {
			httpsEnabled: true,
			manifest:     testLBWebServiceManifestWithBadCount,

			expectedErr: fmt.Errorf("parse task count value badCount: invalid range value badCount. Should be in format of ${min}-${max}"),
		},
	}
	for name, tc := range testCases {
		t.Run(name, func(t *testing.T) {

			// GIVEN
			conf := &LoadBalancedWebService{
				ecsWkld: &ecsWkld{
					wkld: &wkld{
						name: aws.StringValue(tc.manifest.Name),
						env:  testEnvName,
						app:  testAppName,
						rc: RuntimeConfig{
							Image: &ECRImage{
								RepoURL:  testImageRepoURL,
								ImageTag: testImageTag,
							},
						},
					},
					tc: tc.manifest.TaskConfig,
				},
				manifest:             tc.manifest,
				httpsEnabled:         tc.httpsEnabled,
				dnsDelegationEnabled: tc.dnsDelegationEnabled,
			}

			// WHEN
			params, err := conf.Parameters()

			// THEN
			if err == nil {
				require.ElementsMatch(t, tc.expectedParams, params)
			} else {
				require.EqualError(t, tc.expectedErr, err.Error())
			}
		})
	}
}

func TestLoadBalancedWebService_SerializedParameters(t *testing.T) {
	var testLBWebServiceManifest = manifest.NewLoadBalancedWebService(&manifest.LoadBalancedWebServiceProps{
		WorkloadProps: &manifest.WorkloadProps{
			Name:       "frontend",
			Dockerfile: "frontend/Dockerfile",
		},
		Path: "frontend",
		Port: 80,
	})
	testCases := map[string]struct {
		mockDependencies func(ctrl *gomock.Controller, c *LoadBalancedWebService)

		wantedParams string
		wantedError  error
	}{
		"unavailable template": {
			mockDependencies: func(ctrl *gomock.Controller, c *LoadBalancedWebService) {
				m := mocks.NewMockloadBalancedWebSvcReadParser(ctrl)
				m.EXPECT().Parse(wkldParamsTemplatePath, gomock.Any(), gomock.Any()).Return(nil, errors.New("some error"))
				c.wkld.parser = m
			},
			wantedParams: "",
			wantedError:  errors.New("some error"),
		},
		"render params template": {
			mockDependencies: func(ctrl *gomock.Controller, c *LoadBalancedWebService) {
				m := mocks.NewMockloadBalancedWebSvcReadParser(ctrl)
				m.EXPECT().Parse(wkldParamsTemplatePath, gomock.Any(), gomock.Any()).Return(&template.Content{Buffer: bytes.NewBufferString("params")}, nil)
				c.wkld.parser = m
			},
			wantedParams: "params",
		},
	}

	for name, tc := range testCases {
		t.Run(name, func(t *testing.T) {
			// GIVEN
			ctrl := gomock.NewController(t)
			defer ctrl.Finish()
			c := &LoadBalancedWebService{
				ecsWkld: &ecsWkld{
					wkld: &wkld{
						name: aws.StringValue(testLBWebServiceManifest.Name),
						env:  testEnvName,
						app:  testAppName,
						rc: RuntimeConfig{
							Image: &ECRImage{
								RepoURL:  testImageRepoURL,
								ImageTag: testImageTag,
							},
							AdditionalTags: map[string]string{
								"owner": "boss",
							},
						},
					},
					tc: testLBWebServiceManifest.TaskConfig,
				},
				manifest: testLBWebServiceManifest,
			}
			tc.mockDependencies(ctrl, c)

			// WHEN
			params, err := c.SerializedParameters()

			// THEN
			require.Equal(t, tc.wantedError, err)
			require.Equal(t, tc.wantedParams, params)
		})
	}
}

func TestLoadBalancedWebService_Tags(t *testing.T) {
	// GIVEN
	var testLBWebServiceManifest = manifest.NewLoadBalancedWebService(&manifest.LoadBalancedWebServiceProps{
		WorkloadProps: &manifest.WorkloadProps{
			Name:       "frontend",
			Dockerfile: "frontend/Dockerfile",
		},
		Path: "frontend",
		Port: 80,
	})
	conf := &LoadBalancedWebService{
		ecsWkld: &ecsWkld{
			wkld: &wkld{
				name: aws.StringValue(testLBWebServiceManifest.Name),
				env:  testEnvName,
				app:  testAppName,
				rc: RuntimeConfig{
					Image: &ECRImage{
						RepoURL:  testImageRepoURL,
						ImageTag: testImageTag,
					},
					AdditionalTags: map[string]string{
						"owner":              "boss",
						deploy.AppTagKey:     "overrideapp",
						deploy.EnvTagKey:     "overrideenv",
						deploy.ServiceTagKey: "overridesvc",
					},
				},
			},
		},
		manifest: testLBWebServiceManifest,
	}

	// WHEN
	tags := conf.Tags()

	// THEN
	require.Equal(t, []*cloudformation.Tag{
		{
			Key:   aws.String(deploy.AppTagKey),
			Value: aws.String("phonetool"),
		},
		{
			Key:   aws.String(deploy.EnvTagKey),
			Value: aws.String("test"),
		},
		{
			Key:   aws.String(deploy.ServiceTagKey),
			Value: aws.String("frontend"),
		},
		{
			Key:   aws.String("owner"),
			Value: aws.String("boss"),
		},
	}, tags)
}
