// Copyright 2024 The Kubernetes Authors.
//
// Licensed under the Apache License, Version 2.0 (the 'License');
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//    http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an 'AS IS' BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package driver

import (
	"testing"
	"time"

	"github.com/kubernetes-sigs/aws-ebs-csi-driver/pkg/cloud/metadata"
	flag "github.com/spf13/pflag"
)

func TestAddFlags(t *testing.T) {
	o := &Options{}
	o.Mode = AllMode

	f := flag.NewFlagSet("test", flag.ExitOnError)
	o.AddFlags(f)

	if err := f.Set("endpoint", "custom-endpoint"); err != nil {
		t.Errorf("error setting endpoint: %v", err)
	}
	if err := f.Set("http-endpoint", ":8080"); err != nil {
		t.Errorf("error setting http-endpoint: %v", err)
	}
	if err := f.Set("metrics-cert-file", "/https.crt"); err != nil {
		t.Errorf("error setting metrics-cert-file: %v", err)
	}
	if err := f.Set("metrics-key-file", "/https.key"); err != nil {
		t.Errorf("error setting metrics-key-file: %v", err)
	}
	if err := f.Set("enable-otel-tracing", "true"); err != nil {
		t.Errorf("error setting enable-otel-tracing: %v", err)
	}
	if err := f.Set("extra-tags", "key1=value1,key2=value2"); err != nil {
		t.Errorf("error setting extra-tags: %v", err)
	}
	if err := f.Set("k8s-tag-cluster-id", "cluster-123"); err != nil {
		t.Errorf("error setting k8s-tag-cluster-id: %v", err)
	}
	if err := f.Set("aws-sdk-debug-log", "true"); err != nil {
		t.Errorf("error setting aws-sdk-debug-log: %v", err)
	}
	if err := f.Set("deprecated-metrics", "true"); err != nil {
		t.Errorf("error setting deprecated-metrics: %v", err)
	}
	if err := f.Set("warn-on-invalid-tag", "true"); err != nil {
		t.Errorf("error setting warn-on-invalid-tag: %v", err)
	}
	if err := f.Set("user-agent-extra", "extra-info"); err != nil {
		t.Errorf("error setting user-agent-extra: %v", err)
	}
	if err := f.Set("batching", "true"); err != nil {
		t.Errorf("error setting batching: %v", err)
	}
	if err := f.Set("modify-volume-request-handler-timeout", "1m"); err != nil {
		t.Errorf("error setting modify-volume-request-handler-timeout: %v", err)
	}
	if err := f.Set("volume-attach-limit", "10"); err != nil {
		t.Errorf("error setting volume-attach-limit: %v", err)
	}
	if err := f.Set("reserved-volume-attachments", "5"); err != nil {
		t.Errorf("error setting reserved-volume-attachments: %v", err)
	}
	if err := f.Set("legacy-xfs", "true"); err != nil {
		t.Errorf("error setting legacy-xfs: %v", err)
	}

	if err := f.Set("csi-mount-point-prefix", "/var/lib/kubelet"); err != nil {
		t.Errorf("error setting csi-mount-point-prefix: %v", err)
	}

	if o.Endpoint != "custom-endpoint" {
		t.Errorf("unexpected Endpoint: got %s, want custom-endpoint", o.Endpoint)
	}
	if o.HTTPEndpoint != ":8080" {
		t.Errorf("unexpected HTTPEndpoint: got %s, want :8080", o.HTTPEndpoint)
	}
	if !o.EnableOtelTracing {
		t.Error("unexpected EnableOtelTracing: got false, want true")
	}
	if len(o.ExtraTags) != 2 || o.ExtraTags["key1"] != "value1" || o.ExtraTags["key2"] != "value2" {
		t.Errorf("unexpected ExtraTags: got %v, want map[key1:value1 key2:value2]", o.ExtraTags)
	}
	if o.KubernetesClusterID != "cluster-123" {
		t.Errorf("unexpected KubernetesClusterID: got %s, want cluster-123", o.KubernetesClusterID)
	}
	if !o.AwsSdkDebugLog {
		t.Error("unexpected AwsSdkDebugLog: got false, want true")
	}
	if !o.WarnOnInvalidTag {
		t.Error("unexpected WarnOnInvalidTag: got false, want true")
	}
	if o.UserAgentExtra != "extra-info" {
		t.Errorf("unexpected UserAgentExtra: got %s, want extra-info", o.UserAgentExtra)
	}
	if !o.Batching {
		t.Error("unexpected Batching: got false, want true")
	}
	if o.ModifyVolumeRequestHandlerTimeout != time.Minute {
		t.Errorf("unexpected ModifyVolumeRequestHandlerTimeout: got %v, want 1m", o.ModifyVolumeRequestHandlerTimeout)
	}
	if o.VolumeAttachLimit != 10 {
		t.Errorf("unexpected VolumeAttachLimit: got %d, want 10", o.VolumeAttachLimit)
	}
	if o.ReservedVolumeAttachments != 5 {
		t.Errorf("unexpected ReservedVolumeAttachments: got %d, want 5", o.ReservedVolumeAttachments)
	}
	if !o.LegacyXFSProgs {
		t.Errorf("unexpected LegacyXFSProgs: got false, want true")
	}
}

func TestValidateAttachmentLimits(t *testing.T) {
	tests := []struct {
		name                string
		volumeAttachLimit   int64
		reservedAttachments int
		expectedErr         bool
		errMsg              string
	}{
		{
			name:                "both options not set",
			volumeAttachLimit:   -1,
			reservedAttachments: -1,
			expectedErr:         false,
		},
		{
			name:                "volumeAttachLimit set",
			volumeAttachLimit:   10,
			reservedAttachments: -1,
			expectedErr:         false,
		},
		{
			name:                "reservedVolumeAttachments set",
			volumeAttachLimit:   -1,
			reservedAttachments: 2,
			expectedErr:         false,
		},
		{
			name:                "both options set",
			volumeAttachLimit:   10,
			reservedAttachments: 2,
			expectedErr:         true,
			errMsg:              "only one of --volume-attach-limit and --reserved-volume-attachments may be specified",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Add default flags
			o := &Options{}
			o.Mode = NodeMode
			f := flag.NewFlagSet("test", flag.ExitOnError)
			o.AddFlags(f)

			// Override with test flags
			o.VolumeAttachLimit = tt.volumeAttachLimit
			o.ReservedVolumeAttachments = tt.reservedAttachments

			err := o.Validate()
			if (err != nil) != tt.expectedErr {
				t.Errorf("Options.Validate() error = %v, wantErr %v", err, tt.expectedErr)
			}

			if err != nil && err.Error() != tt.errMsg {
				t.Errorf("Options.Validate() error message = %v, wantErrMsg %v", err.Error(), tt.errMsg)
			}
		})
	}
}

func TestValidateMetricsHTTPS(t *testing.T) {
	tests := []struct {
		name            string
		httpEndpoint    string
		metricsCertFile string
		metricsKeyFile  string
		expectError     bool
	}{
		{
			name: "disabled",
		},
		{
			name:         "only http",
			httpEndpoint: ":8080",
		},
		{
			name:            "https with all",
			httpEndpoint:    ":443",
			metricsCertFile: "/https.crt",
			metricsKeyFile:  "/https.key",
		},
		{
			name:            "https with endpoint missing",
			metricsCertFile: "/https.crt",
			metricsKeyFile:  "/https.key",
			expectError:     true,
		},
		{
			name:           "https with cert missing",
			httpEndpoint:   ":443",
			metricsKeyFile: "/https.key",
			expectError:    true,
		},
		{
			name:            "https with key missing",
			httpEndpoint:    ":443",
			metricsCertFile: "/https.crt",
			expectError:     true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Add default flags
			o := &Options{}
			o.Mode = ControllerMode
			f := flag.NewFlagSet("test", flag.ExitOnError)
			o.AddFlags(f)

			// Override with test flags
			o.HTTPEndpoint = tt.httpEndpoint
			o.MetricsCertFile = tt.metricsCertFile
			o.MetricsKeyFile = tt.metricsKeyFile

			err := o.Validate()
			if (err != nil) != tt.expectError {
				t.Errorf("Options.Validate() error = %v, wantErr %v", err, tt.expectError)
			}
		})
	}
}

func TestValidateMetadataSources(t *testing.T) {
	tests := []struct {
		name            string
		metadataSources []string
		expectError     bool
	}{
		{
			name:            "success: default",
			metadataSources: metadata.DefaultMetadataSources,
		},
		{
			name:            "success: imds",
			metadataSources: []string{metadata.SourceIMDS},
		},
		{
			name:            "success: kubernetes",
			metadataSources: []string{metadata.SourceK8s},
		},
		{
			name:            "success: all sources reversed",
			metadataSources: []string{"kubernetes", "imds"},
		},
		{
			name:            "fail: invalid source",
			metadataSources: []string{"invalid"},
			expectError:     true,
		},
		{
			name:            "fail: no source",
			metadataSources: []string{""},
			expectError:     true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Add default flags
			o := &Options{}
			o.Mode = NodeMode
			f := flag.NewFlagSet("test", flag.ExitOnError)
			o.AddFlags(f)

			// Override with test flags
			o.MetadataSources = tt.metadataSources

			err := o.Validate()
			if (err != nil) != tt.expectError {
				t.Errorf("Options.Validate() error = %v, wantErr %v", err, tt.expectError)
			}
		})
	}
}
