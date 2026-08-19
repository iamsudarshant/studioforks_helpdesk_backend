package ticket

// DepartmentAgent is one option in an assign or transfer picker.
//
// The rows themselves come from user.Repository.AssignableStaff, which owns the
// single definition of "may this person be handed a ticket in this department".
// Two implementations of that question lived here and in the user package, and
// they disagreed: this one demanded an explicit department row and so hid every
// generalist, while the assign picker asked nothing and so offered the wrong
// line's desk on every ticket. Only the wire shape is declared here now.
type DepartmentAgent struct {
	ID    string `json:"id"`
	Name  string `json:"name"`
	Email string `json:"email,omitempty"`
	Role  string `json:"role,omitempty"`
}
