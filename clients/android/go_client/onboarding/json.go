package onboarding

import "encoding/json"

func strictMarshal(value any) ([]byte, error) { return json.Marshal(value) }
