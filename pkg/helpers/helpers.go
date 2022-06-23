package helpers

import (
	"encoding/json"
	"fmt"

	"github.com/andygrunwald/go-jira"
)

// GetUnknownField will attempt to get the specified field from the Unknowns struct and unmarshal
// the value into the provided function. If the field is not set, the first return value of this
// function will return false.
func GetUnknownField(field string, issue *jira.Issue, fn func() interface{}) (bool, error) {
	obj := fn()
	if issue.Fields == nil || issue.Fields.Unknowns == nil {
		return false, nil
	}
	unknownField, ok := issue.Fields.Unknowns[field]
	if !ok {
		return false, nil
	}
	bytes, err := json.Marshal(unknownField)
	if err != nil {
		return true, fmt.Errorf("failed to process the custom field %s. Error : %v", field, err)
	}
	if err := json.Unmarshal(bytes, obj); err != nil {
		return true, fmt.Errorf("failed to unmarshall the json to struct for %s. Error: %v", field, err)
	}
	return true, nil

}

// GetIssueSecurityLevel returns the security level of an issue. If no security level
// is set for the issue, the returned SecurityLevel and error will both be nil and
// the issue will follow the default project security level.
func GetIssueSecurityLevel(issue *jira.Issue) (*SecurityLevel, error) {
	// TODO: Add field to the upstream go-jira package; if a security level exists, it is returned
	// as part of the issue fields
	// See https://github.com/andygrunwald/go-jira/issues/456
	var obj *SecurityLevel
	isSet, err := GetUnknownField("security", issue, func() interface{} {
		obj = &SecurityLevel{}
		return obj
	})
	if !isSet {
		return nil, err
	}
	return obj, err
}

type SecurityLevel struct {
	Self        string `json:"self"`
	ID          string `json:"id"`
	Name        string `json:"name"`
	Description string `json:"description"`
}

func GetIssueQaContact(issue *jira.Issue) (*jira.User, error) {
	var obj *jira.User
	isSet, err := GetUnknownField("customfield_12316243", issue, func() interface{} {
		obj = &jira.User{}
		return obj
	})
	if !isSet {
		return nil, err
	}
	return obj, err
}

func GetIssueTargetVersion(issue *jira.Issue) ([]*jira.Version, error) {
	var obj *[]*jira.Version
	isSet, err := GetUnknownField("customfield_12319940", issue, func() interface{} {
		obj = &[]*jira.Version{{}}
		return obj
	})
	if !isSet {
		return nil, err
	}
	return *obj, err
}

func GetIssueSeverity(issue *jira.Issue) (*Severity, error) {
	var obj *Severity
	isSet, err := GetUnknownField("customfield_12316142", issue, func() interface{} {
		obj = &Severity{}
		return obj
	})
	if !isSet {
		return nil, err
	}
	return obj, err
}

type Severity struct {
	Self     string `json:"self"`
	ID       string `json:"id"`
	Value    string `json:"value"`
	Disabled bool   `json:"disabled"`
}
