package user

import (
	"context"
	"encoding/csv"
	"fmt"
	"io"
	"strconv"
	"strings"
	"time"

	"github.com/jmoiron/sqlx"

	"github.com/karmamgmt/complydesk/internal/platform"
)

// Bulk import of an employee roster.
//
// A client arrives with ten thousand employees in a spreadsheet, so this is how
// they get in — not one form at a time. The whole thing is validate-then-import
// rather than a single upload, because a roster with forty bad rows should tell
// you about all forty before it creates the other nine thousand.
//
// Nothing here trusts the file: every row is checked against the same rules the
// single-user form uses, and a row that fails is reported with its line number
// and the offending column rather than aborting the run.

// BulkColumns is the template, in order. The mandatory statutory identifiers
// are required — employee_code, first_name, email, mobile, pan_number,
// uan_number, pf_number, date_of_birth and date_of_joining — and the rest are
// optional but recommended, since they give an employee more ways to sign in.
var BulkColumns = []string{
	"employee_code", "first_name", "last_name", "email",
	"alt_email", "mobile", "pan_number", "uan_number", "pf_number", "esic_number",
	"date_of_birth", "date_of_joining", "designation", "entity_code", "site_code",
	"department_code",
}

// bulkRequiredColumns are the columns a roster must carry. The brief lists them
// as the minimum every employee record needs: the identifiers an account is
// created against, and the date range that anchors employment history.
var bulkRequiredColumns = []string{
	"employee_code", "first_name", "email", "mobile",
	"pan_number", "uan_number", "pf_number", "date_of_birth", "date_of_joining",
}

// BulkRow is one parsed line.
type BulkRow struct {
	Line         int
	EmployeeCode string
	FirstName    string
	LastName     string
	Email        string
	AltEmail     string
	Mobile       string
	PAN          string
	UAN          string
	PF           string
	ESIC         string
	DateOfBirth  string
	DateOfJoin   string
	Designation  string
	EntityCode   string
	SiteCode     string
	DeptCode     string
}

// RowError is one problem with one cell.
type RowError struct {
	Line    int    `json:"row"`
	Column  string `json:"column"`
	Value   string `json:"value,omitempty"`
	Code    string `json:"code"`
	Message string `json:"message"`
}

// BulkResult is what validation and import both return.
type BulkResult struct {
	TotalRows    int        `json:"total_rows"`
	ValidRows    int        `json:"valid_rows"`
	InvalidRows  int        `json:"invalid_rows"`
	ImportedRows int        `json:"imported_rows"`
	Errors       []RowError `json:"errors"`
	// Credentials are returned only by an import, and only once. They are not
	// stored: a list of usable passwords sitting in a table is a standing
	// liability, and the rule below means they can always be reconstructed.
	Credentials []Credential `json:"credentials,omitempty"`
}

// Credential is one new account's first-sign-in detail.
type Credential struct {
	EmployeeCode string `json:"employee_code"`
	Name         string `json:"name"`
	Email        string `json:"email"`
	Password     string `json:"password"`
}

// DefaultPassword builds the first password for a new account: their PAN in
// lower case, an '@', and their birth year — abcde1234f@1990.
//
// Derived rather than random so an administrator can tell somebody what it is
// without a lookup, and always forced to change on first sign-in because anyone
// holding the roster can work it out.
//
// Lower case deliberately: PAN is stored and displayed upper case, and asking
// somebody to type a mixed-case password from a document they are reading in
// capitals is how first sign-ins fail. One case, stated once.
//
// Both parts must be present. Without a PAN or a date of birth there is no
// derivable password, and the caller must handle that rather than silently
// inventing a weak one.
func DefaultPassword(panNumber string, dateOfBirth time.Time) (string, bool) {
	pan := strings.ToLower(strings.TrimSpace(panNumber))
	if pan == "" || dateOfBirth.IsZero() {
		return "", false
	}
	return fmt.Sprintf("%s@%d", pan, dateOfBirth.Year()), true
}

// ParseBulkCSV reads a roster into rows, reporting structural problems rather
// than returning early on the first bad line.
func ParseBulkCSV(r io.Reader) ([]BulkRow, []RowError, error) {
	reader := csv.NewReader(r)
	reader.TrimLeadingSpace = true
	// Rows may legitimately have fewer columns than the header when the
	// trailing optional ones are blank.
	reader.FieldsPerRecord = -1

	records, err := reader.ReadAll()
	if err != nil {
		return nil, nil, fmt.Errorf("reading the roster: %w", err)
	}
	if len(records) == 0 {
		return nil, nil, fmt.Errorf("the file is empty")
	}

	// Map the header so column order does not have to match the template — a
	// client exporting from their own HR system will not match it.
	index := map[string]int{}
	for i, name := range records[0] {
		index[strings.ToLower(strings.TrimSpace(name))] = i
	}
	for _, required := range bulkRequiredColumns {
		if _, ok := index[required]; !ok {
			return nil, nil, fmt.Errorf("the file has no %q column", required)
		}
	}

	at := func(record []string, column string) string {
		i, ok := index[column]
		if !ok || i >= len(record) {
			return ""
		}
		return strings.TrimSpace(record[i])
	}

	rows := make([]BulkRow, 0, len(records)-1)
	errs := []RowError{}

	for n, record := range records[1:] {
		line := n + 2 // 1-based, and the header is line 1
		if len(record) == 0 || strings.TrimSpace(strings.Join(record, "")) == "" {
			continue // blank line, not an error
		}

		row := BulkRow{
			Line: line, EmployeeCode: at(record, "employee_code"),
			FirstName: at(record, "first_name"), LastName: at(record, "last_name"),
			Email: at(record, "email"), AltEmail: at(record, "alt_email"),
			Mobile: at(record, "mobile"), PAN: strings.ToUpper(at(record, "pan_number")),
			UAN: at(record, "uan_number"), PF: at(record, "pf_number"),
			ESIC: at(record, "esic_number"), DateOfBirth: at(record, "date_of_birth"),
			DateOfJoin: at(record, "date_of_joining"), Designation: at(record, "designation"),
			EntityCode: at(record, "entity_code"), SiteCode: at(record, "site_code"),
			DeptCode: at(record, "department_code"),
		}
		rows = append(rows, row)
	}
	return rows, errs, nil
}

// ValidateBulk checks every row against the same rules the single-user form
// applies, plus uniqueness within the file and against the database.
func (r *Repository) ValidateBulk(ctx context.Context, tenantID int64, rows []BulkRow) (*BulkResult, error) {
	out := &BulkResult{TotalRows: len(rows), Errors: []RowError{}}

	// Duplicates inside the file are as much a problem as duplicates against the
	// database, and are easier to miss.
	seenCode := map[string]int{}
	seenEmail := map[string]int{}

	for _, row := range rows {
		bad := false
		add := func(column, value, code, message string) {
			out.Errors = append(out.Errors, RowError{
				Line: row.Line, Column: column, Value: value, Code: code, Message: message,
			})
			bad = true
		}

		if row.EmployeeCode == "" {
			add("employee_code", "", "REQUIRED", "An employee ID is required.")
		}
		if row.FirstName == "" {
			add("first_name", "", "REQUIRED", "A first name is required.")
		}
		if row.Email == "" {
			add("email", "", "REQUIRED", "An email address is required.")
		} else if !strings.Contains(row.Email, "@") || strings.HasPrefix(row.Email, "@") {
			add("email", row.Email, "INVALID", "That is not a valid email address.")
		}
		if row.Mobile == "" {
			add("mobile", "", "REQUIRED", "A mobile number is required.")
		}
		if row.PAN == "" {
			add("pan_number", "", "REQUIRED", "A PAN number is required.")
		} else if len(row.PAN) != 10 {
			add("pan_number", row.PAN, "INVALID", "A PAN number is exactly 10 characters.")
		}
		if row.UAN == "" {
			add("uan_number", "", "REQUIRED", "A UAN number is required.")
		}
		if row.PF == "" {
			add("pf_number", "", "REQUIRED", "A PF number is required.")
		}
		if row.DateOfBirth == "" {
			add("date_of_birth", "", "REQUIRED", "A date of birth is required.")
		}
		if row.DateOfJoin == "" {
			add("date_of_joining", "", "REQUIRED", "A date of joining is required.")
		}

		if first, ok := seenCode[strings.ToLower(row.EmployeeCode)]; ok && row.EmployeeCode != "" {
			add("employee_code", row.EmployeeCode, "DUPLICATE_IN_FILE",
				fmt.Sprintf("The same employee ID is on line %d.", first))
		} else if row.EmployeeCode != "" {
			seenCode[strings.ToLower(row.EmployeeCode)] = row.Line
		}
		if first, ok := seenEmail[strings.ToLower(row.Email)]; ok && row.Email != "" {
			add("email", row.Email, "DUPLICATE_IN_FILE",
				fmt.Sprintf("The same email is on line %d.", first))
		} else if row.Email != "" {
			seenEmail[strings.ToLower(row.Email)] = row.Line
		}

		if _, err := parseBulkDate(row.DateOfBirth); row.DateOfBirth != "" && err != nil {
			add("date_of_birth", row.DateOfBirth, "INVALID", "Use YYYY-MM-DD or DD/MM/YYYY.")
		}
		if _, err := parseBulkDate(row.DateOfJoin); row.DateOfJoin != "" && err != nil {
			add("date_of_joining", row.DateOfJoin, "INVALID", "Use YYYY-MM-DD or DD/MM/YYYY.")
		}

		// The password rule needs both parts. Warn rather than reject: the row
		// still imports, the account just cannot be signed into until someone
		// sets a password or sends a reset.
		dob, _ := parseBulkDate(row.DateOfBirth)
		if _, ok := DefaultPassword(row.PAN, dob); !ok {
			out.Errors = append(out.Errors, RowError{
				Line: row.Line, Column: "pan_number", Code: "NO_DEFAULT_PASSWORD",
				Message: "No PAN or date of birth, so no first password can be derived. " +
					"The account will be created without one and will need a reset link.",
			})
		}

		if !bad {
			out.ValidRows++
		}
	}
	out.InvalidRows = out.TotalRows - out.ValidRows

	// Clashes with people who already exist. Done in one query rather than one
	// per row, so a ten thousand row file is still a single round trip.
	if err := r.flagExistingBulk(ctx, tenantID, rows, out); err != nil {
		return nil, err
	}
	return out, nil
}

// flagExistingBulk marks rows whose employee code or email is already taken.
func (r *Repository) flagExistingBulk(ctx context.Context, tenantID int64, rows []BulkRow, out *BulkResult) error {
	codes, emails := []string{}, []string{}
	for _, row := range rows {
		if row.EmployeeCode != "" {
			codes = append(codes, row.EmployeeCode)
		}
		if row.Email != "" {
			emails = append(emails, row.Email)
		}
	}
	if len(codes) == 0 && len(emails) == 0 {
		return nil
	}

	type hit struct {
		Code  string `db:"employee_code"`
		Email string `db:"email"`
	}
	found := []hit{}

	query, args, err := sqlx.In(`
		SELECT COALESCE(employee_code,'') AS employee_code, COALESCE(email,'') AS email
		FROM users
		WHERE tenant_id = ? AND deleted_at IS NULL
		  AND (employee_code IN (?) OR email IN (?))`,
		tenantID, orPlaceholder(codes), orPlaceholder(emails))
	if err != nil {
		return fmt.Errorf("building the existing-user check: %w", err)
	}
	if err := r.db.Primary.SelectContext(ctx, &found, r.db.Primary.Rebind(query), args...); err != nil {
		return fmt.Errorf("checking for existing users: %w", err)
	}

	takenCode, takenEmail := map[string]bool{}, map[string]bool{}
	for _, h := range found {
		takenCode[strings.ToLower(h.Code)] = true
		takenEmail[strings.ToLower(h.Email)] = true
	}

	for _, row := range rows {
		if row.EmployeeCode != "" && takenCode[strings.ToLower(row.EmployeeCode)] {
			out.Errors = append(out.Errors, RowError{
				Line: row.Line, Column: "employee_code", Value: row.EmployeeCode,
				Code: "ALREADY_EXISTS", Message: "An employee already has this ID.",
			})
			out.ValidRows--
		} else if row.Email != "" && takenEmail[strings.ToLower(row.Email)] {
			out.Errors = append(out.Errors, RowError{
				Line: row.Line, Column: "email", Value: row.Email,
				Code: "ALREADY_EXISTS", Message: "An employee already has this email.",
			})
			out.ValidRows--
		}
	}
	if out.ValidRows < 0 {
		out.ValidRows = 0
	}
	out.InvalidRows = out.TotalRows - out.ValidRows
	return nil
}

// orPlaceholder keeps sqlx.In happy when one of the lists is empty: an empty
// IN () is a syntax error, and a value that cannot match is the safe stand-in.
func orPlaceholder(values []string) []string {
	if len(values) == 0 {
		return []string{"\x00-none-\x00"}
	}
	return values
}

// parseBulkDate accepts the two formats an HR export realistically produces.
func parseBulkDate(value string) (time.Time, error) {
	value = strings.TrimSpace(value)
	if value == "" {
		return time.Time{}, nil
	}
	for _, layout := range []string{"2006-01-02", "02/01/2006", "02-01-2006", "2006/01/02"} {
		if t, err := time.Parse(layout, value); err == nil {
			return t, nil
		}
	}
	return time.Time{}, fmt.Errorf("unrecognised date %q", value)
}

// BulkTemplate renders the CSV header plus one example row, so somebody filling
// it in can see the expected shape of every column.
func BulkTemplate() []byte {
	var b strings.Builder
	b.WriteString(strings.Join(BulkColumns, ","))
	b.WriteString("\n")
	b.WriteString(strings.Join([]string{
		"EMP-1001", "Anita", "Desai", "anita.desai@example.com",
		"anita@personal.example", "9800000001", "ABCDE1234F", "100234567890",
		"MHBAN00123450000012345", "3100012345", "1994-06-15", "2022-04-01",
		"Accounts Executive", "AMP-MFG", "AMP-MUM", "FIN",
	}, ","))
	b.WriteString("\n")
	return []byte(b.String())
}

// ImportBulk creates the valid rows and returns each new account's first
// password.
//
// The whole import runs in one transaction: a roster that half-imports leaves an
// administrator unable to tell who exists and who does not, and re-running it
// would then collide on the rows that succeeded.
func (r *Repository) ImportBulk(
	ctx context.Context, tenantID int64, rows []BulkRow,
	hash func(string) (string, error), groupID, roleID int64, actorID *int64,
) (*BulkResult, error) {
	result, err := r.ValidateBulk(ctx, tenantID, rows)
	if err != nil {
		return nil, err
	}

	// Only rows with no blocking error are imported. NO_DEFAULT_PASSWORD is a
	// warning, so it does not disqualify a row.
	blocked := map[int]bool{}
	for _, e := range result.Errors {
		if e.Code != "NO_DEFAULT_PASSWORD" {
			blocked[e.Line] = true
		}
	}

	err = r.db.InTx(ctx, func(tx *sqlx.Tx) error {
		for _, row := range rows {
			if blocked[row.Line] {
				continue
			}

			dob, _ := parseBulkDate(row.DateOfBirth)
			doj, _ := parseBulkDate(row.DateOfJoin)

			var passwordHash *string
			password, derivable := DefaultPassword(row.PAN, dob)
			if derivable {
				h, err := hash(password)
				if err != nil {
					return fmt.Errorf("hashing the password for %s: %w", row.EmployeeCode, err)
				}
				passwordHash = &h
			}

			res, err := tx.ExecContext(ctx, `
				INSERT INTO users
					(public_id, tenant_id, employee_code, first_name, last_name, email,
					 alt_email, mobile, pan_number, uan_number, pf_number, esic_number,
					 date_of_birth, date_of_joining, designation, user_group_id,
					 password_hash, password_algo, must_change_password, status, created_by)
				VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,'argon2id',1,'ACTIVE',?)`,
				platform.NewULID(), tenantID, row.EmployeeCode, row.FirstName,
				nullIfBlank(row.LastName), nullIfBlank(row.Email), nullIfBlank(row.AltEmail),
				nullIfBlank(row.Mobile), nullIfBlank(row.PAN), nullIfBlank(row.UAN),
				nullIfBlank(row.PF), nullIfBlank(row.ESIC),
				nullTime(dob), nullTime(doj), nullIfBlank(row.Designation),
				groupID, passwordHash, actorID)
			if err != nil {
				return fmt.Errorf("creating %s (line %d): %w", row.EmployeeCode, row.Line, err)
			}

			id, err := res.LastInsertId()
			if err != nil {
				return fmt.Errorf("reading the new user id: %w", err)
			}
			if _, err := tx.ExecContext(ctx,
				`INSERT INTO user_roles (user_id, role_id) VALUES (?,?)`, id, roleID); err != nil {
				return fmt.Errorf("assigning the employee role on line %d: %w", row.Line, err)
			}

			result.ImportedRows++
			if derivable {
				result.Credentials = append(result.Credentials, Credential{
					EmployeeCode: row.EmployeeCode,
					Name:         strings.TrimSpace(row.FirstName + " " + row.LastName),
					Email:        row.Email,
					Password:     password,
				})
			}
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return result, nil
}

func nullIfBlank(s string) any {
	if strings.TrimSpace(s) == "" {
		return nil
	}
	return s
}

func nullTime(t time.Time) any {
	if t.IsZero() {
		return nil
	}
	return t
}

// atoi is used by the handler for row limits; kept here so the parsing rules
// stay with the rest of the bulk code.
func atoi(s string, fallback int) int {
	if n, err := strconv.Atoi(strings.TrimSpace(s)); err == nil && n > 0 {
		return n
	}
	return fallback
}
