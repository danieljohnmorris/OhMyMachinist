package issues

import (
	"os"
)

type Config struct {
	Label string `toml:"label"`
	Plane map[string]PlaneProject `toml:"plane"`
	Linear map[string]LinearProject `toml:"linear"`
}

type PlaneProject struct {
	ProjectID string `toml:"project_id"`
}

type LinearProject struct {
	Project string `toml:"project"`
}

func DefaultLabel() string {
	if label := os.Getenv("MACHINIST_ISSUE_LABEL"); label != "" {
		return label
	}
	return "factory"
}

// PlaneProjects maps configured project IDs to repositories and loads shared
// connection settings from environment variables.
func (c Config) PlaneProjects() map[string]PlaneConnection {
	connections := map[string]PlaneConnection{}
	for repository, project := range c.Plane {
		connections[repository] = PlaneConnection{
			URL:       os.Getenv("PLANE_URL"),
			Workspace: os.Getenv("PLANE_WORKSPACE"),
			APIKey:    os.Getenv("PLANE_API_KEY"),
			ProjectID: project.ProjectID,
		}
	}
	return connections
}

// LinearProjects maps project names to repositories and reads the shared API key.
func (c Config) LinearProjects() map[string]LinearConnection {
	connections := map[string]LinearConnection{}
	for repository, project := range c.Linear {
		connections[repository] = LinearConnection{Project: project.Project, APIKey: os.Getenv("LINEAR_API_KEY")}
	}
	return connections
}

type PlaneConnection struct {
	URL       string
	Workspace string
	ProjectID string
	APIKey    string
}

type LinearConnection struct {
	Project string
	APIKey  string
}

func NewPlaneConnection(connection PlaneConnection) (*PlaneClient, error) {
	return NewPlaneClient(PlaneConfig{
		URL: connection.URL, Workspace: connection.Workspace,
		ProjectID: connection.ProjectID, APIKey: connection.APIKey,
	})
}

func NewLinearConnection(connection LinearConnection) (*LinearClient, error) {
	return NewLinearClient(LinearConfig{APIKey: connection.APIKey, Project: connection.Project})
}
