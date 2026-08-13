package credential

// service and account name the item in whichever platform store is in use.
//
// The scheme is shared across platforms deliberately: a user who moves a
// workspace between machines should find the item under the same name, and a
// user auditing their credential store should be able to see at a glance which
// items this program owns and delete them without a tool.
func service(ref Ref) string {
	return "switchboard:" + ref.Provider
}

func account(ref Ref) string {
	if ref.Account == "" {
		return "default"
	}
	return ref.Account
}
