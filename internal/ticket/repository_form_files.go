package ticket

// Documents chosen inside a category's own form.
//
// A category can declare a FILE field — "Supporting document" is one every PF
// and ESI category ships with — and the browser uploads to /documents and puts
// the resulting id into `custom_fields`. That is where it stayed: the document
// row existed, the ticket recorded its id as a value, and nothing ever wrote a
// `ticket_attachments` row. So the file was uploaded, stored, charged against
// quota and then invisible — the ticket showed a 26-character identifier where
// a filename should have been, the Attachments tab was empty, and nobody could
// open what the requester had sent.
//
// The dropzone at the foot of the form did link its files, which is why the two
// halves of the same screen behaved differently. This makes the form's FILE
// fields behave like the dropzone: the ids are read out of the values, resolved
// against the ticket's own client, and attached.

import (
	"context"
	"fmt"

	"github.com/karmamgmt/complydesk/internal/platform"
)

// FileFieldKeys returns the custom-field keys of type FILE for a category,
// including the ones a subcategory inherits from its parent.
//
// Inheritance matters because a subcategory carries no fields of its own — see
// catalogue.FieldsInherited — so asking only about the leaf finds nothing and
// every attachment on a subcategory ticket would be dropped.
func (r *Repository) FileFieldKeys(ctx context.Context, tenantID, categoryID int64) ([]string, error) {
	keys := []string{}
	err := r.db.Primary.SelectContext(ctx, &keys, `
		SELECT DISTINCT f.field_key
		FROM category_fields f
		JOIN categories c ON c.id = ? AND c.tenant_id = ?
		WHERE f.tenant_id = ?
		  AND f.field_type = 'FILE'
		  AND f.is_active = 1
		  AND (f.category_id = c.id OR f.category_id = c.parent_id)`,
		categoryID, tenantID, tenantID)
	if err != nil {
		return nil, fmt.Errorf("loading file field keys: %w", err)
	}
	return keys, nil
}

// DocumentRefsInFields pulls the document references out of a set of submitted
// custom-field values, given the keys that hold files.
//
// A FILE field's value is either one reference or a list of them, depending on
// whether the field allows multiples, so both shapes are accepted. Anything
// that is not a well-formed public id is ignored rather than rejected: a field
// whose value is a filename typed by hand is a bad value, not a reason to
// refuse the ticket.
func DocumentRefsInFields(fields map[string]any, fileKeys []string) []string {
	if len(fields) == 0 || len(fileKeys) == 0 {
		return nil
	}

	seen := map[string]bool{}
	out := []string{}

	add := func(value any) {
		ref, ok := value.(string)
		if !ok || !platform.ValidULID(ref) || seen[ref] {
			return
		}
		seen[ref] = true
		out = append(out, ref)
	}

	for _, key := range fileKeys {
		switch value := fields[key].(type) {
		case nil:
			continue
		case []any:
			for _, item := range value {
				add(item)
			}
		default:
			add(value)
		}
	}
	return out
}

// mergeRefs appends the second list to the first, dropping repeats. A requester
// who put the same file in the dropzone and in the form field meant to send it
// once.
func mergeRefs(first, second []string) []string {
	seen := make(map[string]bool, len(first))
	out := make([]string, 0, len(first)+len(second))
	for _, ref := range append(append([]string{}, first...), second...) {
		if ref == "" || seen[ref] {
			continue
		}
		seen[ref] = true
		out = append(out, ref)
	}
	return out
}
