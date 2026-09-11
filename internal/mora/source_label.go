package mora

// sourceLabel returns the human name the app shows for a configured source.
func sourceLabel(s Source) string {
	switch s.Type {
	case "imessage":
		return "Messages"
	case "applecalendar":
		return "Apple Calendar"
	case "gmail", "calendar":
		label := "Gmail"
		if s.Type == "calendar" {
			label = "Google Calendar"
		}
		account := s.Email
		if account == "" {
			account = s.Account
		}
		if account != "" {
			label += " · " + account
		}
		return label
	case "filesystem":
		return "Folder · " + s.Name
	case "github":
		return "GitHub"
	case "whatsapp":
		return "WhatsApp"
	default:
		return s.Name
	}
}
