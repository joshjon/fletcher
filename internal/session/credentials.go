package session

import (
	"github.com/joshjon/fletcher/internal/job"
)

// SavedCredentials lists the box's saved logins (those with files under the
// credentials root). Today that is the git login - agent login seeding was
// removed (a frozen OAuth snapshot goes stale, see docs/ROADMAP.md M16).
func (m *Manager) SavedCredentials() []string {
	return job.SavedCredentials(m.opt().CredentialsRoot)
}

// SaveGitCredential saves a git host login (host + username + token, plus an
// optional committer identity) under the box's credentials root, so new
// sessions seeded with the "git" credential clone over HTTPS. No running
// session is needed - the credential is built from the given fields.
func (m *Manager) SaveGitCredential(host, username, token, gitName, gitEmail string) error {
	return job.WriteGitCredential(m.opt().CredentialsRoot, host, username, token, gitName, gitEmail)
}

// DeleteSavedCredential removes a saved login from the credentials root.
func (m *Manager) DeleteSavedCredential(name string) error {
	return job.DeleteSavedCredential(m.opt().CredentialsRoot, name)
}
