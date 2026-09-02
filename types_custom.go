package modellink

import (
	"bytes"
	"encoding/json"
	"errors"
)

type Interleaved struct {
	Field string
}

func (value *Interleaved) UnmarshalJSON(data []byte) error {
	data = bytes.TrimSpace(data)
	if bytes.Equal(data, []byte("true")) {
		value.Field = ""
		return nil
	}
	var object struct {
		Field string `json:"field"`
	}
	if err := json.Unmarshal(data, &object); err != nil {
		return errors.New("modellink: interleaved must be true or an object")
	}
	if object.Field != "reasoning_content" && object.Field != "reasoning_details" {
		return errors.New("modellink: interleaved contains an unsupported field")
	}
	value.Field = object.Field
	return nil
}

func (value Interleaved) MarshalJSON() ([]byte, error) {
	if value.Field == "" {
		return []byte("true"), nil
	}
	if value.Field != "reasoning_content" && value.Field != "reasoning_details" {
		return nil, errors.New("modellink: interleaved contains an unsupported field")
	}
	return json.Marshal(struct {
		Field string `json:"field"`
	}{Field: value.Field})
}
