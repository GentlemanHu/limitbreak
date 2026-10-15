package domain

import "time"

// CatalogEntry is one Claude desktop Code session as listed in the app's own
// session index. Only identity and display metadata are carried; prompts,
// summaries and permissions are never read into it.
type CatalogEntry struct {
	HostSessionID  string
	CLISessionID   string
	Cwd            string
	Title          string
	Model          string
	Archived       bool
	LastActivityAt time.Time
}

// ValidCatalogEntry accepts entries whose identifiers can be routed exactly.
func ValidCatalogEntry(c CatalogEntry) bool {
	return ValidHostSessionID(c.HostSessionID) && ValidCLISessionID(c.CLISessionID) &&
		(c.Cwd == "" || ValidAbsolutePath(c.Cwd)) && plainField(c.Title, 500) && plainField(c.Model, 128)
}
