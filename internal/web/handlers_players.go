package web

import (
	"encoding/csv"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"

	"badminton/internal/store"
)

func (s *Server) handlePlayers(w http.ResponseWriter, r *http.Request) {
	search := strings.TrimSpace(r.URL.Query().Get("q"))
	players, err := s.store.Players(r.Context(), search)
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	active := 0
	for _, p := range players {
		if p.Mailable() {
			active++
		}
	}
	s.renderAdmin(w, r, "players.html", view{
		Title: "Mailing list",
		Page:  "players",
		Data: map[string]any{
			"Players":     players,
			"Search":      search,
			"ActiveCount": active,
		},
	})
}

func (s *Server) handlePlayerCreate(w http.ResponseWriter, r *http.Request) {
	name := strings.TrimSpace(r.PostFormValue("name"))
	email := strings.TrimSpace(r.PostFormValue("email"))
	if name == "" || !looksLikeEmail(email) {
		s.fail(w, r, "A name and a valid email address are both needed.", "/players")
		return
	}
	if _, err := s.store.CreatePlayer(r.Context(), name, email, true); err != nil {
		if errors.Is(err, store.ErrDuplicateEmail) {
			s.fail(w, r, email+" is already on the list.", "/players")
			return
		}
		s.serverError(w, r, err)
		return
	}
	s.ok(w, r, name+" added to the mailing list.", "/players")
}

func (s *Server) handlePlayerUpdate(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		s.notFound(w, r)
		return
	}
	name := strings.TrimSpace(r.PostFormValue("name"))
	email := strings.TrimSpace(r.PostFormValue("email"))
	active := r.PostFormValue("active") != ""
	if name == "" || !looksLikeEmail(email) {
		s.fail(w, r, "A name and a valid email address are both needed.", "/players")
		return
	}
	if err := s.store.UpdatePlayer(r.Context(), id, name, email, active); err != nil {
		if errors.Is(err, store.ErrDuplicateEmail) {
			s.fail(w, r, email+" belongs to someone else on the list.", "/players")
			return
		}
		s.serverError(w, r, err)
		return
	}
	// Editing someone back onto the list should also undo an old unsubscribe,
	// otherwise they silently keep missing every email.
	if active {
		if err := s.store.SetUnsubscribed(r.Context(), id, false); err != nil {
			s.serverError(w, r, err)
			return
		}
	}
	s.ok(w, r, name+" updated.", "/players")
}

// handlePlayerDelete removes someone from the mailing list. If they have any
// RSVP history it deactivates them instead of deleting, so past sessions keep
// their rosters intact.
func (s *Server) handlePlayerDelete(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		s.notFound(w, r)
		return
	}
	ctx := r.Context()
	player, err := s.store.Player(ctx, id)
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	if player == nil {
		s.notFound(w, r)
		return
	}

	answered, err := s.store.PlayerHasHistory(ctx, id)
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	if answered {
		if err := s.store.UpdatePlayer(ctx, id, player.Name, player.Email, false); err != nil {
			s.serverError(w, r, err)
			return
		}
		s.ok(w, r, player.Name+" was removed from the mailing list. Their past sessions are kept.", "/players")
		return
	}
	if err := s.store.DeletePlayer(ctx, id); err != nil {
		s.serverError(w, r, err)
		return
	}
	s.ok(w, r, player.Name+" deleted.", "/players")
}

// handlePlayerImport accepts a pasted list or an uploaded CSV and upserts by
// email, so re-importing the same file updates names instead of duplicating.
func (s *Server) handlePlayerImport(w http.ResponseWriter, r *http.Request) {
	var reader io.Reader
	if file, _, err := r.FormFile("file"); err == nil {
		defer file.Close()
		reader = io.LimitReader(file, 2<<20)
	} else if pasted := strings.TrimSpace(r.PostFormValue("pasted")); pasted != "" {
		reader = strings.NewReader(pasted)
	} else {
		s.fail(w, r, "Choose a CSV file or paste a list first.", "/players")
		return
	}

	people, warnings, err := parsePeopleCSV(reader)
	if err != nil {
		s.fail(w, r, "That file could not be read: "+err.Error(), "/players")
		return
	}
	if len(people) == 0 {
		s.fail(w, r, "No rows with an email address were found. Use two columns: name, email.", "/players")
		return
	}

	res, err := s.store.ImportPlayers(r.Context(), people)
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	msg := fmt.Sprintf("Imported: %d added, %d updated", res.Added, res.Updated)
	if skipped := res.Skipped + warnings; skipped > 0 {
		msg += fmt.Sprintf(", %d skipped", skipped)
	}
	s.ok(w, r, msg+".", "/players")
}

// parsePeopleCSV reads name/email pairs. It tolerates a header row, either
// column order, and one-column lists of bare email addresses, because the file
// will come out of whatever the organizer already has.
func parsePeopleCSV(r io.Reader) (people []struct{ Name, Email string }, skipped int, err error) {
	cr := csv.NewReader(r)
	cr.FieldsPerRecord = -1
	cr.TrimLeadingSpace = true

	rows, err := cr.ReadAll()
	if err != nil {
		return nil, 0, err
	}
	for i, row := range rows {
		var name, email string
		for _, field := range row {
			field = strings.TrimSpace(field)
			if field == "" {
				continue
			}
			if strings.Contains(field, "@") && email == "" {
				email = field
			} else if name == "" {
				name = field
			}
		}
		if email == "" || !looksLikeEmail(email) {
			// A first row without a usable email is almost certainly the header.
			if i > 0 {
				skipped++
			}
			continue
		}
		if name == "" {
			name = strings.SplitN(email, "@", 2)[0]
		}
		people = append(people, struct{ Name, Email string }{name, email})
	}
	return people, skipped, nil
}
