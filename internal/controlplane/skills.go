package controlplane

import (
	"context"
	"errors"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strings"

	"github.com/owainlewis/machinist/internal/skills"
)

// The skills endpoints expose the versioned skills library on disk
// (internal/skills) to the documented API and the MCP tools. Reads need a read
// token; installs, updates, pins and removals need a write token and write an
// audit entry. Recorded sources never carry credentials, so responses are safe
// for any authenticated caller.

// maxSkillUploadBytes bounds one multipart upload. The library enforces the
// per-file and total limits when it stages the version.
const maxSkillUploadBytes = 16 << 20

// skillView is one skill in an API or MCP response: the library entry plus the
// projects and review profiles that reference it.
type skillView struct {
	Name     string           `json:"name"`
	Source   string           `json:"source"`
	Active   string           `json:"active"`
	Versions []skills.Version `json:"versions"`
	UsedBy   []string         `json:"used_by"`
}

// openSkills opens the versioned skills library at the default root
// (<data>/skills).
func (s *Server) openSkills() (*skills.Library, error) {
	library, err := skills.Open("")
	if err != nil {
		return nil, err
	}
	library.Warn = func(message string) { log.Printf("skills: %s", message) }
	return library, nil
}

// skillUsedBy lists the projects and review profiles that reference a skill.
// Reference tracking arrives with the skills configuration work (MACH-50); the
// field is always present and empty until then.
func (s *Server) skillUsedBy(string) []string { return []string{} }

func (s *Server) newSkillView(name, source, active string, versions []skills.Version) skillView {
	if versions == nil {
		versions = []skills.Version{}
	}
	return skillView{Name: name, Source: source, Active: active, Versions: versions, UsedBy: s.skillUsedBy(name)}
}

// listSkills returns every installed skill, sorted by name.
func (s *Server) listSkills() ([]skillView, error) {
	library, err := s.openSkills()
	if err != nil {
		return nil, err
	}
	installed, err := library.List()
	if err != nil {
		return nil, err
	}
	views := make([]skillView, 0, len(installed))
	for _, skill := range installed {
		views = append(views, s.newSkillView(skill.Name, skill.Source, skill.Active, skill.Versions))
	}
	return views, nil
}

// getSkill returns one installed skill with all of its versions.
func (s *Server) getSkill(name string) (skillView, error) {
	library, err := s.openSkills()
	if err != nil {
		return skillView{}, err
	}
	manifest, err := library.Versions(name)
	if err != nil {
		return skillView{}, err
	}
	return s.newSkillView(manifest.Name, manifest.Source, manifest.Active, manifest.Versions), nil
}

// installSkill resolves a URL, directory or zip source and installs it.
func (s *Server) installSkill(ctx context.Context, source string) (skills.InstallResult, error) {
	parsed, err := skills.ParseSource(source)
	if err != nil {
		return skills.InstallResult{}, err
	}
	library, err := s.openSkills()
	if err != nil {
		return skills.InstallResult{}, err
	}
	return library.Install(ctx, parsed)
}

// installSkillArchive installs an uploaded zip archive or a bare SKILL.md by
// staging the bytes in a temporary directory the library can read. The install
// records an "upload:" sentinel source, not the staging path: the staged bytes
// are removed once the version is written, so a later update reports that the
// skill must be re-uploaded.
func (s *Server) installSkillArchive(ctx context.Context, filename string, content []byte) (skills.InstallResult, error) {
	directory, err := os.MkdirTemp("", "machinist-skill-upload-*")
	if err != nil {
		return skills.InstallResult{}, err
	}
	defer os.RemoveAll(directory)
	base := filepath.Base(strings.TrimSpace(filename))
	var parsed skills.Source
	switch {
	case strings.EqualFold(base, "SKILL.md"):
		parsed = skills.Source{Kind: skills.SourceDirectory, Location: directory}
		if err := os.WriteFile(filepath.Join(directory, "SKILL.md"), content, 0o600); err != nil {
			return skills.InstallResult{}, err
		}
	case strings.EqualFold(filepath.Ext(base), ".zip"):
		archive := filepath.Join(directory, "upload.zip")
		if err := os.WriteFile(archive, content, 0o600); err != nil {
			return skills.InstallResult{}, err
		}
		parsed = skills.Source{Kind: skills.SourceZip, Location: archive}
	default:
		return skills.InstallResult{}, errors.New("upload must be a .zip archive or a SKILL.md file")
	}
	parsed.Uploaded = true
	library, err := s.openSkills()
	if err != nil {
		return skills.InstallResult{}, err
	}
	return library.Install(ctx, parsed)
}

// updateSkill re-resolves a skill's recorded source and adds the new version.
func (s *Server) updateSkill(ctx context.Context, name string, activate bool) (skills.InstallResult, error) {
	library, err := s.openSkills()
	if err != nil {
		return skills.InstallResult{}, err
	}
	return library.Update(ctx, name, activate)
}

// pinSkill sets the active version of an installed skill.
func (s *Server) pinSkill(name, version string) error {
	library, err := s.openSkills()
	if err != nil {
		return err
	}
	return library.Pin(name, version)
}

// uninstallSkill removes a skill and every installed version.
func (s *Server) uninstallSkill(name string) error {
	library, err := s.openSkills()
	if err != nil {
		return err
	}
	return library.Uninstall(name, skills.UninstallOptions{})
}

// recordSkillAudit writes one audit entry for a skills change made over MCP,
// where there is no HTTP request to read the principal from.
func (s *Server) recordSkillAudit(ctx context.Context, identity TokenIdentity, action, detail string) {
	if err := s.store.RecordAudit(ctx, identity, action, detail); err != nil {
		log.Printf("audit %s: %v", action, err)
	}
}

// writeSkillError maps a library error onto an HTTP response: a skill missing
// from the library is 404, anything else uses the supplied status.
func writeSkillError(response http.ResponseWriter, err error, status int) {
	if errors.Is(err, skills.ErrNotInstalled) {
		writeError(response, http.StatusNotFound, errors.New("skill not found"))
		return
	}
	writeError(response, status, err)
}

// skillPathValue returns the validated {name} path value. A name that could not
// address a library directory is rejected before it reaches the filesystem.
func skillPathValue(response http.ResponseWriter, request *http.Request) (string, bool) {
	name := request.PathValue("name")
	if _, err := skills.Key(name); err != nil {
		writeError(response, http.StatusBadRequest, errors.New("skill name is not usable"))
		return "", false
	}
	return name, true
}

// decodeOptionalJSON decodes an optional JSON body into target; an absent body
// is not an error, so a POST with no payload keeps its zero-value input.
func decodeOptionalJSON(request *http.Request, target any) error {
	if request.ContentLength == 0 {
		return nil
	}
	return decodeJSON(request, target)
}

// listSkillsHandler serves GET /api/v1/skills.
func (s *Server) listSkillsHandler(response http.ResponseWriter, request *http.Request) {
	views, err := s.listSkills()
	if err != nil {
		writeError(response, http.StatusInternalServerError, err)
		return
	}
	response.Header().Set("Cache-Control", "no-store")
	writeJSON(response, http.StatusOK, map[string]any{"skills": views})
}

// getSkillHandler serves GET /api/v1/skills/{name}.
func (s *Server) getSkillHandler(response http.ResponseWriter, request *http.Request) {
	name, ok := skillPathValue(response, request)
	if !ok {
		return
	}
	view, err := s.getSkill(name)
	if err != nil {
		writeSkillError(response, err, http.StatusInternalServerError)
		return
	}
	response.Header().Set("Cache-Control", "no-store")
	writeJSON(response, http.StatusOK, view)
}

// installSkillHandler serves POST /api/v1/skills with {"source": "…"}.
func (s *Server) installSkillHandler(response http.ResponseWriter, request *http.Request) {
	if !limitRequestBody(response, request, maxRequestBytes) {
		return
	}
	var input struct {
		Source string `json:"source"`
	}
	if err := decodeJSON(request, &input); err != nil {
		writeDecodeError(response, err)
		return
	}
	source := strings.TrimSpace(input.Source)
	if source == "" {
		writeError(response, http.StatusBadRequest, errors.New("skill source is required"))
		return
	}
	result, err := s.installSkill(request.Context(), source)
	if err != nil {
		writeSkillError(response, err, http.StatusBadRequest)
		return
	}
	s.recordAudit(request, "skills.install", result.Name)
	writeJSON(response, http.StatusCreated, result)
}

// uploadSkillHandler serves POST /api/v1/skills/upload as a multipart form with
// one "file" part holding a .zip archive or a bare SKILL.md.
func (s *Server) uploadSkillHandler(response http.ResponseWriter, request *http.Request) {
	if !limitRequestBody(response, request, maxSkillUploadBytes) {
		return
	}
	if err := request.ParseMultipartForm(maxSkillUploadBytes); err != nil {
		writeDecodeError(response, err)
		return
	}
	defer request.MultipartForm.RemoveAll()
	file, header, err := request.FormFile("file")
	if err != nil {
		writeError(response, http.StatusBadRequest, errors.New(`multipart field "file" is required`))
		return
	}
	defer file.Close()
	content, err := io.ReadAll(io.LimitReader(file, maxSkillUploadBytes+1))
	if err != nil {
		writeDecodeError(response, err)
		return
	}
	if len(content) > maxSkillUploadBytes {
		writeError(response, http.StatusRequestEntityTooLarge, errors.New("skill upload is too large"))
		return
	}
	result, err := s.installSkillArchive(request.Context(), header.Filename, content)
	if err != nil {
		writeSkillError(response, err, http.StatusBadRequest)
		return
	}
	s.recordAudit(request, "skills.upload", result.Name)
	writeJSON(response, http.StatusCreated, result)
}

// updateSkillHandler serves POST /api/v1/skills/{name}/update with an optional
// {"activate": true}.
func (s *Server) updateSkillHandler(response http.ResponseWriter, request *http.Request) {
	name, ok := skillPathValue(response, request)
	if !ok {
		return
	}
	if !limitRequestBody(response, request, maxRequestBytes) {
		return
	}
	var input struct {
		Activate bool `json:"activate"`
	}
	if err := decodeOptionalJSON(request, &input); err != nil {
		writeDecodeError(response, err)
		return
	}
	result, err := s.updateSkill(request.Context(), name, input.Activate)
	if err != nil {
		writeSkillError(response, err, http.StatusBadRequest)
		return
	}
	s.recordAudit(request, "skills.update", name)
	writeJSON(response, http.StatusOK, result)
}

// pinSkillHandler serves POST /api/v1/skills/{name}/pin with {"version": "…"}.
func (s *Server) pinSkillHandler(response http.ResponseWriter, request *http.Request) {
	name, ok := skillPathValue(response, request)
	if !ok {
		return
	}
	if !limitRequestBody(response, request, maxRequestBytes) {
		return
	}
	var input struct {
		Version string `json:"version"`
	}
	if err := decodeJSON(request, &input); err != nil {
		writeDecodeError(response, err)
		return
	}
	version := strings.TrimSpace(input.Version)
	if version == "" {
		writeError(response, http.StatusBadRequest, errors.New("version is required"))
		return
	}
	if err := s.pinSkill(name, version); err != nil {
		writeSkillError(response, err, http.StatusBadRequest)
		return
	}
	s.recordAudit(request, "skills.pin", name+" "+version)
	view, err := s.getSkill(name)
	if err != nil {
		writeSkillError(response, err, http.StatusInternalServerError)
		return
	}
	writeJSON(response, http.StatusOK, view)
}

// uninstallSkillHandler serves DELETE /api/v1/skills/{name}.
func (s *Server) uninstallSkillHandler(response http.ResponseWriter, request *http.Request) {
	name, ok := skillPathValue(response, request)
	if !ok {
		return
	}
	if err := s.uninstallSkill(name); err != nil {
		writeSkillError(response, err, http.StatusInternalServerError)
		return
	}
	s.recordAudit(request, "skills.uninstall", name)
	response.WriteHeader(http.StatusNoContent)
}
