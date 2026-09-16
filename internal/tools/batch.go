package tools

import (
	"encoding/json"
	"strconv"
)

type batchItem struct {
	Index          int    `json:"index"`
	Status         string `json:"status"`
	Error          string `json:"error,omitempty"`
	PreviewOmitted bool   `json:"preview_omitted,omitempty"`
}

type batchReport struct {
	LogicalOperations int         `json:"logical_operations"`
	InternalSubcalls  int         `json:"internal_subcalls"`
	Items             []batchItem `json:"items"`
}

func applyBatchReport(result ToolResult, report batchReport) ToolResult {
	if result.Metadata == nil {
		result.Metadata = make(map[string]string)
	}
	status := "complete"
	for _, item := range report.Items {
		if item.Status != "complete" {
			status = "partial"
			break
		}
	}
	reportJSON, _ := json.Marshal(report)
	result.Metadata["batch"] = string(reportJSON)
	result.Metadata["batch_status"] = status
	result.Metadata["batch_logical_operations"] = strconv.Itoa(report.LogicalOperations)
	result.Metadata["batch_internal_subcalls"] = strconv.Itoa(report.InternalSubcalls)
	return result
}
