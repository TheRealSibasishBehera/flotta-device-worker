// Code generated by go-swagger; DO NOT EDIT.

package models

// This file was generated by the swagger tool.
// Editing this file might prove futile when you re-run the swagger generate command

import (
	"github.com/go-openapi/errors"
	"github.com/go-openapi/strfmt"
	"github.com/go-openapi/swag"
)

// SystemMetricsConfiguration System metrics gathering configuration
//
// swagger:model system-metrics-configuration
type SystemMetricsConfiguration struct {

	// allow list
	AllowList *MetricsAllowList `json:"allow_list,omitempty"`

	// When true, turns system metrics collection off. False by default.
	Disabled bool `json:"disabled,omitempty"`

	// Interval(in seconds) to scrape metrics endpoint.
	Interval int32 `json:"interval,omitempty"`
}

// Validate validates this system metrics configuration
func (m *SystemMetricsConfiguration) Validate(formats strfmt.Registry) error {
	var res []error

	if err := m.validateAllowList(formats); err != nil {
		res = append(res, err)
	}

	if len(res) > 0 {
		return errors.CompositeValidationError(res...)
	}
	return nil
}

func (m *SystemMetricsConfiguration) validateAllowList(formats strfmt.Registry) error {

	if swag.IsZero(m.AllowList) { // not required
		return nil
	}

	if m.AllowList != nil {
		if err := m.AllowList.Validate(formats); err != nil {
			if ve, ok := err.(*errors.Validation); ok {
				return ve.ValidateName("allow_list")
			}
			return err
		}
	}

	return nil
}

// MarshalBinary interface implementation
func (m *SystemMetricsConfiguration) MarshalBinary() ([]byte, error) {
	if m == nil {
		return nil, nil
	}
	return swag.WriteJSON(m)
}

// UnmarshalBinary interface implementation
func (m *SystemMetricsConfiguration) UnmarshalBinary(b []byte) error {
	var res SystemMetricsConfiguration
	if err := swag.ReadJSON(b, &res); err != nil {
		return err
	}
	*m = res
	return nil
}
