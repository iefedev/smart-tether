package pc

import (
	"encoding/json"
	"time"

	"smarttether/internal/link"
)

// PhoneInfo asks the phone for its name (and how its previous process ended)
// over the link. If the phone closes the stream without an answer, the name
// stays empty.
func PhoneInfo(l *Link) (link.PhoneInfo, error) {
	var info link.PhoneInfo
	s, err := l.Open()
	if err != nil {
		return info, err
	}
	defer s.Close()
	s.SetDeadline(time.Now().Add(5 * time.Second))
	if err := link.WriteRequest(s, link.KindInfo, ""); err != nil {
		return info, err
	}
	err = json.NewDecoder(s).Decode(&info)
	return info, err
}
