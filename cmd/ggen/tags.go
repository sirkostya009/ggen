package main

import "strings"

func parseValidationTag(tag string) []ValidationRule {
	if tag == "" {
		return nil
	}
	var rules []ValidationRule
	for _, part := range strings.Split(tag, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		name, value, _ := strings.Cut(part, "=")
		rules = append(rules, ValidationRule{Name: name, Value: value})
	}
	return rules
}

func parseJSONTag(tag string) (name string, ignored bool) {
	if tag == "" {
		return "", false
	}
	name, _, _ = strings.Cut(tag, ",")
	if name == "-" {
		return "", true
	}
	return name, false
}
