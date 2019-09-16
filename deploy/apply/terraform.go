// Copyright 2019 Google LLC
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

package apply

import (
	"errors"
	"fmt"
	"io/ioutil"
	"os"

	"github.com/GoogleCloudPlatform/healthcare/deploy/config"
	"github.com/GoogleCloudPlatform/healthcare/deploy/config/tfconfig"
	"github.com/GoogleCloudPlatform/healthcare/deploy/terraform"
)

func createProjectTerraform(config *config.Config, project *config.Project) error {
	fid := project.FolderID
	if fid == "" {
		fid = config.Overall.FolderID
	}

	// Only one of folder ID or org ID can be set in the project resource.
	var oid string
	if fid == "" {
		oid = config.Overall.OrganizationID
	}

	ba := project.BillingAccount
	if ba == "" {
		ba = config.Overall.BillingAccount
	}

	res := &tfconfig.ProjectResource{
		OrgID:          oid,
		FolderID:       fid,
		BillingAccount: ba,
	}
	if err := res.Init(project.ID); err != nil {
		return err
	}

	tfConf := terraform.NewConfig()
	opts := &terraform.Options{}
	addResources(tfConf, opts, res)

	dir, err := ioutil.TempDir("", "")
	if err != nil {
		return err
	}
	defer os.RemoveAll(dir)

	return terraformApply(tfConf, dir, opts)
}

func defaultTerraform(config *config.Config, project *config.Project) error {
	// State bucket has to be deployed first as other deployments will reference it.
	if err := stateBucket(project); err != nil {
		return fmt.Errorf("failed to apply terraform state: %v", err)
	}

	if err := auditResources(config, project); err != nil {
		return fmt.Errorf("failed to apply audit resources: %v", err)
	}

	// Services deployment enables services that other resources require, so should come before the major resource deployments.
	if err := services(project); err != nil {
		return fmt.Errorf("failed to apply services: %v", err)
	}

	if err := userResources(project); err != nil {
		return fmt.Errorf("failed to apply user resources: %v", err)
	}

	// Default deployment references user deployment, so should be done after the user deployment.
	if err := defaultResources(project); err != nil {
		return fmt.Errorf("failed to apply default resources: %v", err)
	}
	return nil
}

func stateBucket(project *config.Project) error {
	if project.TerraformConfig.StateBucket == nil {
		return errors.New("state_storage_bucket must not be nil")
	}

	tfConf := terraform.NewConfig()
	opts := &terraform.Options{}
	addResources(tfConf, opts, project.TerraformConfig.StateBucket)

	dir, err := ioutil.TempDir("", "")
	if err != nil {
		return err
	}
	defer os.RemoveAll(dir)

	return terraformApply(tfConf, dir, opts)
}

func services(project *config.Project) error {
	if project.Services == nil {
		return nil
	}

	tfConf := terraform.NewConfig()
	tfConf.Terraform.Backend = &terraform.Backend{
		Bucket: project.TerraformConfig.StateBucket.Name,
		Prefix: "services",
	}

	opts := &terraform.Options{}
	if err := addResources(tfConf, opts, project.Services); err != nil {
		return err
	}

	dir, err := ioutil.TempDir("", "")
	if err != nil {
		return err
	}
	defer os.RemoveAll(dir)

	return terraformApply(tfConf, dir, opts)
}

func auditResources(config *config.Config, project *config.Project) error {
	auditProject := config.ProjectForAuditLogs(project)
	tfConf := terraform.NewConfig()
	tfConf.Terraform.Backend = &terraform.Backend{
		Bucket: auditProject.TerraformConfig.StateBucket.Name,
		// Attach project ID to prefix.
		// This is because the audit project will contain an audit deployment for every project it holds audit logs for in its own state bucket.
		// Attaching the prefix ensures we don't have a collision in names.
		Prefix: "audit-" + project.ID,
	}

	d := project.Audit.LogsBigqueryDataset
	d.Accesses = append(d.Accesses, &tfconfig.Access{
		Role: "WRITER",

		// Trim "serviceAccount:" prefix as user_by_email does not allow it.
		UserByEmail: fmt.Sprintf(
			`${replace(google_logging_project_sink.%s.writer_identity, "serviceAccount:", "")}`,
			project.BQLogSinkTF.ID(),
		),
	})

	rs := []tfconfig.Resource{project.BQLogSinkTF, d}
	if b := project.Audit.LogsStorageBucket; b != nil {
		rs = append(rs, b)
	}

	opts := &terraform.Options{}
	addResources(tfConf, opts, rs...)

	dir, err := ioutil.TempDir("", "")
	if err != nil {
		return err
	}
	defer os.RemoveAll(dir)

	if err := terraformApply(tfConf, dir, opts); err != nil {
		return err
	}

	project.GeneratedFields.LogSinkServiceAccount, err = getLogSinkServiceAccount(project, project.BQLogSinkTF.Name)
	if err != nil {
		return fmt.Errorf("failed to get log sink service account: %v", err)
	}
	return nil
}

func userResources(project *config.Project) error {
	rs := project.UserResources()
	tfConf := terraform.NewConfig()
	tfConf.Terraform.Backend = &terraform.Backend{
		Bucket: project.TerraformConfig.StateBucket.Name,
		Prefix: "user",
	}
	opts := &terraform.Options{}
	if err := addResources(tfConf, opts, rs...); err != nil {
		return err
	}

	dir, err := ioutil.TempDir("", "")
	if err != nil {
		return err
	}
	defer os.RemoveAll(dir)

	return terraformApply(tfConf, dir, opts)
}

func defaultResources(project *config.Project) error {
	rs := project.DefaultResources()
	tfConf := terraform.NewConfig()
	tfConf.Terraform.Backend = &terraform.Backend{
		Bucket: project.TerraformConfig.StateBucket.Name,
		Prefix: "default",
	}

	// Allow default deployment to reference user deployment.
	tfConf.Data = append(tfConf.Data, &terraform.Resource{
		Name: "user",
		Type: "terraform_remote_state",
		Properties: map[string]interface{}{
			"backend": "gcs",
			"config": map[string]interface{}{
				"bucket": project.TerraformConfig.StateBucket.Name,
				"prefix": "user",
			},
		},
	})

	opts := &terraform.Options{}
	addResources(tfConf, opts, rs...)

	dir, err := ioutil.TempDir("", "")
	if err != nil {
		return err
	}
	defer os.RemoveAll(dir)

	return terraformApply(tfConf, dir, opts)
}

// addResources adds the given resources to the given terraform config.
// If the resource implements functions to get an import ID, the terraform options will be updated
// with the import ID, so the resource is imported to the terraform state if it already exists.
// The import ID for a resource can be found in terraform's documentation for the resource.
func addResources(config *terraform.Config, opts *terraform.Options, resources ...tfconfig.Resource) error {
	for _, r := range resources {
		config.Resources = append(config.Resources, &terraform.Resource{
			Name:       r.ID(),
			Type:       r.ResourceType(),
			Properties: r,
		})

		type importer interface {
			ImportID() (string, error)
		}

		if i, ok := r.(importer); ok {
			id, err := i.ImportID()
			if err != nil {
				return fmt.Errorf("failed to get import ID for %q %q: %v", r.ResourceType(), r.ID(), err)
			}
			if id != "" {
				opts.Imports = append(opts.Imports, terraform.Import{
					Address: fmt.Sprintf("%s.%s", r.ResourceType(), r.ID()),
					ID:      id,
				})
			}

		}

		type depender interface {
			DependentResources() []tfconfig.Resource
		}
		if d, ok := r.(depender); ok {
			if err := addResources(config, opts, d.DependentResources()...); err != nil {
				return err
			}
		}
	}
	return nil
}
