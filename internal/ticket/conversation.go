package ticket

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/jmoiron/sqlx"

	"github.com/karmamgmt/complydesk/internal/platform"
)

// Conversations returns a ticket's thread.
//
// includeInternal is decided from the caller's permissions, and the filter is
// applied in SQL rather than by dropping rows afterwards: an internal note must
// never travel to an employee or partner even in a serialisation slip.
func (r *Repository) Conversations(ctx context.Context, tenantID, ticketID int64, includeInternal bool) ([]Conversation, error) {
	where := "cv.tenant_id = ? AND cv.ticket_id = ? AND cv.deleted_at IS NULL"
	if !includeInternal {
		where += " AND cv.visibility = 'PUBLIC'"
	}

	rows := []Conversation{}
	q := `SELECT cv.id, cv.public_id, cv.tenant_id, cv.ticket_id, cv.author_id,
	             cv.author_role, cv.visibility, cv.body_html, cv.body_text,
	             cv.is_system, cv.in_reply_to_id, cv.mentions_json, cv.edited_at,
	             cv.created_at,
	             CONCAT(u.first_name, ' ', COALESCE(u.last_name, '')) AS author_name
	      FROM ticket_conversations cv
	      LEFT JOIN users u ON u.id = cv.author_id
	      WHERE ` + where + ` ORDER BY cv.created_at, cv.id`

	if err := r.db.Primary.SelectContext(ctx, &rows, q, tenantID, ticketID); err != nil {
		return nil, fmt.Errorf("loading conversations: %w", err)
	}
	return rows, nil
}

// ReplyParams describes a new conversation entry.
type ReplyParams struct {
	Visibility  string
	BodyHTML    string
	BodyText    string
	AuthorID    *int64
	AuthorName  string
	AuthorRole  string
	Mentions    []string
	DocumentIDs []int64
	IsSystem    bool
}

// Reply appends to the thread, attaches any documents, stamps the first-response
// time and records the timeline entry — all in one transaction so the thread and
// the trail cannot diverge.
func (r *Repository) Reply(ctx context.Context, tenantID, ticketID int64, p ReplyParams) (*Conversation, error) {
	var created *Conversation

	err := r.db.InTx(ctx, func(tx *sqlx.Tx) error {
		visibility := p.Visibility
		if visibility != VisibilityInternal {
			visibility = VisibilityPublic
		}

		var mentions any
		if len(p.Mentions) > 0 {
			raw, err := json.Marshal(p.Mentions)
			if err != nil {
				return fmt.Errorf("encoding mentions: %w", err)
			}
			mentions = string(raw)
		}

		publicID := platform.NewULID()
		res, err := tx.ExecContext(ctx, `
			INSERT INTO ticket_conversations
				(public_id, tenant_id, ticket_id, author_id, author_role, visibility,
				 body_html, body_text, is_system, mentions_json)
			VALUES (?,?,?,?,?,?,?,?,?,?)`,
			publicID, tenantID, ticketID, p.AuthorID, nullStr(p.AuthorRole), visibility,
			nullStr(p.BodyHTML), nullStr(p.BodyText), p.IsSystem, mentions)
		if err != nil {
			return fmt.Errorf("creating reply: %w", err)
		}

		conversationID, err := res.LastInsertId()
		if err != nil {
			return fmt.Errorf("reading reply id: %w", err)
		}

		for _, docID := range p.DocumentIDs {
			if _, err := tx.ExecContext(ctx, `
				INSERT INTO ticket_attachments
					(tenant_id, ticket_id, conversation_id, document_id, context, uploaded_by)
				VALUES (?,?,?,?,?,?)`,
				tenantID, ticketID, conversationID, docID,
				attachmentContext(p.AuthorRole), p.AuthorID); err != nil {
				return fmt.Errorf("attaching document to reply: %w", err)
			}
		}

		// A public reply from staff is the first response the SLA measures, and
		// only the first one counts.
		if visibility == VisibilityPublic && !p.IsSystem {
			if _, err := tx.ExecContext(ctx, `
				UPDATE tickets
				SET first_responded_at = COALESCE(first_responded_at, UTC_TIMESTAMP(3)),
				    last_activity_at = UTC_TIMESTAMP(3)
				WHERE tenant_id = ? AND id = ?`, tenantID, ticketID); err != nil {
				return fmt.Errorf("stamping first response: %w", err)
			}
		} else if _, err := tx.ExecContext(ctx,
			`UPDATE tickets SET last_activity_at = UTC_TIMESTAMP(3)
			 WHERE tenant_id = ? AND id = ?`, tenantID, ticketID); err != nil {
			return fmt.Errorf("touching ticket activity: %w", err)
		}

		event, summary, timelineVisibility := EventReplied, "Replied", VisibilityPublic
		if visibility == VisibilityInternal {
			event, summary, timelineVisibility = EventInternalNote, "Internal note added", VisibilityInternal
		}

		if err := writeTimeline(ctx, tx, tenantID, ticketID, timelineParams{
			EventType: event, ActorID: p.AuthorID, ActorName: p.AuthorName,
			ActorRole: p.AuthorRole, Visibility: timelineVisibility, Summary: summary,
			Detail: map[string]any{"attachments": len(p.DocumentIDs)},
		}); err != nil {
			return err
		}

		var cv Conversation
		if err := tx.GetContext(ctx, &cv, `
			SELECT cv.id, cv.public_id, cv.tenant_id, cv.ticket_id, cv.author_id,
			       cv.author_role, cv.visibility, cv.body_html, cv.body_text,
			       cv.is_system, cv.in_reply_to_id, cv.mentions_json, cv.edited_at,
			       cv.created_at,
			       CONCAT(u.first_name, ' ', COALESCE(u.last_name, '')) AS author_name
			FROM ticket_conversations cv
			LEFT JOIN users u ON u.id = cv.author_id
			WHERE cv.id = ?`, conversationID); err != nil {
			return fmt.Errorf("reloading reply: %w", err)
		}
		created = &cv
		return nil
	})

	return created, err
}

// attachmentContext records who supplied a file, which the attachments tab
// groups by.
func attachmentContext(role string) string {
	switch role {
	case "EMPLOYEE":
		return "REQUESTER"
	case "KARMA_SUPER_ADMIN", "SUPER_ADMIN", "HELPDESK_MASTER_ADMIN":
		return "ADMIN"
	case "":
		return "SYSTEM"
	default:
		return "AGENT"
	}
}

// Attachments lists a ticket's files with their document metadata.
func (r *Repository) Attachments(ctx context.Context, tenantID, ticketID int64) ([]Attachment, error) {
	rows := []Attachment{}
	err := r.db.Primary.SelectContext(ctx, &rows, `
		SELECT ta.id, ta.document_id, doc.public_id AS document_public_id,
		       ta.conversation_id, ta.context, doc.original_name, doc.mime_type,
		       doc.size_bytes, ta.uploaded_by,
		       CONCAT(u.first_name, ' ', COALESCE(u.last_name, '')) AS uploader_name,
		       ta.created_at
		FROM ticket_attachments ta
		JOIN documents doc ON doc.id = ta.document_id
		LEFT JOIN users u  ON u.id = ta.uploaded_by
		WHERE ta.tenant_id = ? AND ta.ticket_id = ? AND doc.deleted_at IS NULL
		ORDER BY ta.created_at DESC`, tenantID, ticketID)
	if err != nil {
		return nil, fmt.Errorf("loading attachments: %w", err)
	}
	return rows, nil
}

// AttachDocuments links already-uploaded documents to a ticket. Used by the
// admin "upload a document against any ticket" flow.
func (r *Repository) AttachDocuments(ctx context.Context, tenantID, ticketID int64, documentIDs []int64, context string, actorID *int64, actorName string) error {
	if len(documentIDs) == 0 {
		return nil
	}

	return r.db.InTx(ctx, func(tx *sqlx.Tx) error {
		for _, docID := range documentIDs {
			// Confirm the document belongs to this client before linking it, or
			// an attachment could reference another client's file.
			var owned int
			if err := tx.GetContext(ctx, &owned,
				`SELECT COUNT(*) FROM documents WHERE tenant_id = ? AND id = ? AND deleted_at IS NULL`,
				tenantID, docID); err != nil {
				return fmt.Errorf("verifying document ownership: %w", err)
			}
			if owned == 0 {
				return platform.ErrSentinelNotFound
			}

			if _, err := tx.ExecContext(ctx, `
				INSERT INTO ticket_attachments
					(tenant_id, ticket_id, document_id, context, uploaded_by)
				VALUES (?,?,?,?,?)
				ON DUPLICATE KEY UPDATE context = VALUES(context)`,
				tenantID, ticketID, docID, context, actorID); err != nil {
				return fmt.Errorf("attaching document: %w", err)
			}
		}

		if _, err := tx.ExecContext(ctx,
			`UPDATE tickets SET last_activity_at = UTC_TIMESTAMP(3)
			 WHERE tenant_id = ? AND id = ?`, tenantID, ticketID); err != nil {
			return fmt.Errorf("touching ticket activity: %w", err)
		}

		return writeTimeline(ctx, tx, tenantID, ticketID, timelineParams{
			EventType: EventAttachmentAdded, ActorID: actorID, ActorName: actorName,
			Summary: fmt.Sprintf("%d document(s) attached", len(documentIDs)),
			Detail:  map[string]any{"count": len(documentIDs), "context": context},
		})
	})
}

// SetFeedback records the requester's satisfaction rating.
func (r *Repository) SetFeedback(ctx context.Context, tenantID, ticketID, userID int64, score int, comment string) error {
	return r.db.InTx(ctx, func(tx *sqlx.Tx) error {
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO ticket_feedback (tenant_id, ticket_id, user_id, score, comment)
			VALUES (?,?,?,?,?)
			ON DUPLICATE KEY UPDATE score = VALUES(score), comment = VALUES(comment)`,
			tenantID, ticketID, userID, score, nullStr(comment)); err != nil {
			return fmt.Errorf("recording feedback: %w", err)
		}

		if _, err := tx.ExecContext(ctx,
			`UPDATE tickets SET csat_score = ?, csat_comment = ? WHERE tenant_id = ? AND id = ?`,
			score, nullStr(comment), tenantID, ticketID); err != nil {
			return fmt.Errorf("updating ticket feedback: %w", err)
		}

		return writeTimeline(ctx, tx, tenantID, ticketID, timelineParams{
			EventType: EventFeedbackGiven, ActorID: &userID,
			Summary: fmt.Sprintf("Feedback submitted: %d of 5", score),
			Detail:  map[string]any{"score": score},
		})
	})
}

// --- thread maintenance -----------------------------------------------------

// ConversationByPublicID loads one message of a ticket's thread.
//
// The ticket id is part of the lookup, not checked afterwards: a message id
// from one ticket must not resolve against another the caller happens to be
// allowed to open.
func (r *Repository) ConversationByPublicID(ctx context.Context, tenantID, ticketID int64, publicID string) (*Conversation, error) {
	var cv Conversation
	err := r.db.Primary.GetContext(ctx, &cv, `
		SELECT cv.id, cv.public_id, cv.tenant_id, cv.ticket_id, cv.author_id,
		       cv.author_role, cv.visibility, cv.body_html, cv.body_text,
		       cv.is_system, cv.in_reply_to_id, cv.mentions_json, cv.edited_at,
		       cv.created_at,
		       CONCAT(u.first_name, ' ', COALESCE(u.last_name, '')) AS author_name
		FROM ticket_conversations cv
		LEFT JOIN users u ON u.id = cv.author_id
		WHERE cv.tenant_id = ? AND cv.ticket_id = ? AND cv.public_id = ?
		  AND cv.deleted_at IS NULL`, tenantID, ticketID, publicID)
	if err != nil {
		if platform.IsNotFound(err) {
			return nil, platform.ErrSentinelNotFound
		}
		return nil, fmt.Errorf("loading a reply: %w", err)
	}
	return &cv, nil
}

// EditConversation rewrites a message body and stamps the edit.
//
// The original is not kept in this table — the timeline entry below is the
// record that it changed, and by whom.
func (r *Repository) EditConversation(ctx context.Context, tenantID, ticketID, conversationID int64, bodyHTML, bodyText string, actorID *int64, actorName string) error {
	return r.db.InTx(ctx, func(tx *sqlx.Tx) error {
		res, err := tx.ExecContext(ctx, `
			UPDATE ticket_conversations
			SET body_html = ?, body_text = ?, edited_at = UTC_TIMESTAMP(3)
			WHERE tenant_id = ? AND ticket_id = ? AND id = ? AND deleted_at IS NULL`,
			nullStr(bodyHTML), nullStr(bodyText), tenantID, ticketID, conversationID)
		if err != nil {
			return fmt.Errorf("editing a reply: %w", err)
		}
		if n, _ := res.RowsAffected(); n == 0 {
			return platform.ErrSentinelNotFound
		}

		return writeTimeline(ctx, tx, tenantID, ticketID, timelineParams{
			EventType: EventReplied, ActorID: actorID, ActorName: actorName,
			Visibility: VisibilityInternal, Summary: "Reply edited",
		})
	})
}

// DeleteConversation withdraws a message from the thread.
//
// Soft only. A reply that was visible to an employee is part of what they were
// told, and a helpdesk that can erase its own words leaves no trail worth
// auditing.
func (r *Repository) DeleteConversation(ctx context.Context, tenantID, ticketID, conversationID int64, actorID *int64, actorName string) error {
	return r.db.InTx(ctx, func(tx *sqlx.Tx) error {
		res, err := tx.ExecContext(ctx, `
			UPDATE ticket_conversations SET deleted_at = UTC_TIMESTAMP(3)
			WHERE tenant_id = ? AND ticket_id = ? AND id = ? AND deleted_at IS NULL`,
			tenantID, ticketID, conversationID)
		if err != nil {
			return fmt.Errorf("deleting a reply: %w", err)
		}
		if n, _ := res.RowsAffected(); n == 0 {
			return platform.ErrSentinelNotFound
		}

		return writeTimeline(ctx, tx, tenantID, ticketID, timelineParams{
			EventType: EventReplied, ActorID: actorID, ActorName: actorName,
			Visibility: VisibilityInternal, Summary: "Reply withdrawn",
		})
	})
}

// MarkConversationRead records that someone has seen a message.
func (r *Repository) MarkConversationRead(ctx context.Context, conversationID, userID int64) error {
	_, err := r.db.Primary.ExecContext(ctx, `
		INSERT INTO ticket_conversation_reads (conversation_id, user_id)
		VALUES (?,?)
		ON DUPLICATE KEY UPDATE read_at = read_at`, conversationID, userID)
	if err != nil {
		return fmt.Errorf("recording a read receipt: %w", err)
	}
	return nil
}

// ReadConversationIDs returns which of a ticket's messages one person has seen,
// so the thread can be rendered with unread markers in a single round trip.
func (r *Repository) ReadConversationIDs(ctx context.Context, tenantID, ticketID, userID int64) (map[int64]bool, error) {
	ids := []int64{}
	err := r.db.Primary.SelectContext(ctx, &ids, `
		SELECT cr.conversation_id
		FROM ticket_conversation_reads cr
		JOIN ticket_conversations cv ON cv.id = cr.conversation_id
		WHERE cv.tenant_id = ? AND cv.ticket_id = ? AND cr.user_id = ?`,
		tenantID, ticketID, userID)
	if err != nil {
		return nil, fmt.Errorf("loading read receipts: %w", err)
	}

	out := make(map[int64]bool, len(ids))
	for _, id := range ids {
		out[id] = true
	}
	return out, nil
}

// DetachDocument removes one file from a ticket.
//
// The document itself is left alone: it may be attached to another ticket, and
// deleting the file is a separate, separately-permissioned act.
func (r *Repository) DetachDocument(ctx context.Context, tenantID, ticketID, attachmentID int64, actorID *int64, actorName string) error {
	return r.db.InTx(ctx, func(tx *sqlx.Tx) error {
		var name string
		err := tx.GetContext(ctx, &name, `
			SELECT doc.original_name FROM ticket_attachments ta
			JOIN documents doc ON doc.id = ta.document_id
			WHERE ta.tenant_id = ? AND ta.ticket_id = ? AND ta.id = ?`,
			tenantID, ticketID, attachmentID)
		if err != nil {
			if platform.IsNotFound(err) {
				return platform.ErrSentinelNotFound
			}
			return fmt.Errorf("loading the attachment: %w", err)
		}

		if _, err := tx.ExecContext(ctx,
			`DELETE FROM ticket_attachments WHERE tenant_id = ? AND ticket_id = ? AND id = ?`,
			tenantID, ticketID, attachmentID); err != nil {
			return fmt.Errorf("detaching the document: %w", err)
		}

		return writeTimeline(ctx, tx, tenantID, ticketID, timelineParams{
			EventType: EventAttachmentAdded, ActorID: actorID, ActorName: actorName,
			Summary: "Attachment removed: " + name,
			Detail:  map[string]any{"file_name": name},
		})
	})
}

// AttachmentsByConversation groups a ticket's files by the message they arrived
// with, so the thread can show each reply's own attachments.
func (r *Repository) AttachmentsByConversation(ctx context.Context, tenantID, ticketID int64) (map[int64][]Attachment, error) {
	rows, err := r.Attachments(ctx, tenantID, ticketID)
	if err != nil {
		return nil, err
	}

	out := map[int64][]Attachment{}
	for _, row := range rows {
		if row.ConversationID.Valid {
			out[row.ConversationID.Int64] = append(out[row.ConversationID.Int64], row)
		}
	}
	return out, nil
}

// OpeningAttachments returns the files that arrived with the ticket itself.
//
// A file uploaded while raising a ticket belongs to the description, not to any
// reply — there is no conversation row yet when it is stored, so it groups onto
// no message and used to appear only in the Attachments tab. Somebody reading
// the thread then saw a request that referred to a document with no document in
// sight, and had to go looking in another tab to find out what was sent and
// when. These belong under the opening post, which is where they were put.
func (r *Repository) OpeningAttachments(ctx context.Context, tenantID, ticketID int64) ([]Attachment, error) {
	rows, err := r.Attachments(ctx, tenantID, ticketID)
	if err != nil {
		return nil, err
	}

	out := []Attachment{}
	for _, row := range rows {
		if !row.ConversationID.Valid {
			out = append(out, row)
		}
	}
	return out, nil
}
