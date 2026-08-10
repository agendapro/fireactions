package server

import (
	"context"

	"github.com/hostinger/fireactions/helper/github"

	githubv63 "github.com/google/go-github/v63/github"
)

// githubAPI is the narrow slice of the GitHub App API that Pool uses for the
// just-in-time runner flow. It exists so pool lifecycle can be exercised in
// tests without a live GitHub App; *githubAppClient is the only production
// implementation.
type githubAPI interface {
	FindOrganizationInstallation(ctx context.Context, org string) (int64, error)
	GenerateOrgJITConfig(ctx context.Context, installationID int64, org, name string, groupID int64, labels []string) (jitConfig string, runnerID int64, runnerName string, err error)
	RemoveOrganizationRunner(ctx context.Context, installationID int64, org string, runnerID int64) error
}

// githubAppClient adapts *github.Client (helper/github's GitHub App wrapper)
// to githubAPI. It is pure delegation to the same underlying calls: Pool
// already wraps their errors with "github: %w", so no additional wrapping
// happens here.
type githubAppClient struct {
	client *github.Client
}

// newGitHubAppClient creates a new githubAppClient.
func newGitHubAppClient(client *github.Client) *githubAppClient {
	return &githubAppClient{client: client}
}

var _ githubAPI = (*githubAppClient)(nil)

// FindOrganizationInstallation implements githubAPI.
func (c *githubAppClient) FindOrganizationInstallation(ctx context.Context, org string) (int64, error) {
	installation, _, err := c.client.Apps.FindOrganizationInstallation(ctx, org)
	if err != nil {
		return 0, err
	}

	return installation.GetID(), nil
}

// GenerateOrgJITConfig implements githubAPI.
func (c *githubAppClient) GenerateOrgJITConfig(ctx context.Context, installationID int64, org, name string, groupID int64, labels []string) (string, int64, string, error) {
	jitConfig, _, err := c.client.Installation(installationID).Actions.GenerateOrgJITConfig(ctx, org, &githubv63.GenerateJITConfigRequest{
		Name:          name,
		RunnerGroupID: groupID,
		Labels:        labels,
	})
	if err != nil {
		return "", 0, "", err
	}

	return jitConfig.GetEncodedJITConfig(), jitConfig.GetRunner().GetID(), jitConfig.GetRunner().GetName(), nil
}

// RemoveOrganizationRunner implements githubAPI.
func (c *githubAppClient) RemoveOrganizationRunner(ctx context.Context, installationID int64, org string, runnerID int64) error {
	_, err := c.client.Installation(installationID).Actions.RemoveOrganizationRunner(ctx, org, runnerID)
	return err
}
