// Copyright IBM Corp. 2025
// SPDX-License-Identifier: MPL-2.0

package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/hashicorp/go-tfe"
	"github.com/hashicorp/terraform-mcp-server/pkg/client"
	"github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"
	log "github.com/sirupsen/logrus"
)

// GetCurrentState creates a tool to retrieve the current Terraform state for a workspace.
func GetCurrentState(logger *log.Logger) server.ServerTool {
	return server.ServerTool{
		Tool: mcp.NewTool("get_current_state",
			mcp.WithDescription(`Retrieves the current Terraform state for a workspace from Terraform Cloud/Enterprise. Returns the state version metadata (serial, status, terraform version, resource summary) and all state outputs. Optionally downloads the full JSON state representation for detailed resource inspection.`),
			mcp.WithTitleAnnotation("Get current Terraform state for a workspace"),
			mcp.WithReadOnlyHintAnnotation(true),
			mcp.WithDestructiveHintAnnotation(false),
			mcp.WithString("terraform_org_name",
				mcp.Required(),
				mcp.Description("The Terraform Cloud/Enterprise organization name"),
			),
			mcp.WithString("workspace_name",
				mcp.Required(),
				mcp.Description("The name of the workspace to retrieve state for"),
			),
			mcp.WithString("include_full_state",
				mcp.Description("Whether to include the full JSON state download. Set to 'true' for detailed resource attributes. Defaults to 'false' to return only metadata and outputs."),
				mcp.DefaultString("false"),
			),
		),
		Handler: func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			return getCurrentStateHandler(ctx, req, logger)
		},
	}
}

func getCurrentStateHandler(ctx context.Context, request mcp.CallToolRequest, logger *log.Logger) (*mcp.CallToolResult, error) {
	terraformOrgName, err := request.RequireString("terraform_org_name")
	if err != nil {
		return ToolError(logger, "missing required input: terraform_org_name", err)
	}
	terraformOrgName = strings.TrimSpace(terraformOrgName)

	workspaceName, err := request.RequireString("workspace_name")
	if err != nil {
		return ToolError(logger, "missing required input: workspace_name", err)
	}
	workspaceName = strings.TrimSpace(workspaceName)

	includeFullState := strings.ToLower(strings.TrimSpace(request.GetString("include_full_state", "false"))) == "true"

	tfeClient, err := client.GetTfeClientFromContext(ctx, logger)
	if err != nil {
		return ToolError(logger, "failed to get Terraform client", err)
	}

	// Look up the workspace to get its ID
	workspace, err := tfeClient.Workspaces.Read(ctx, terraformOrgName, workspaceName)
	if err != nil {
		return ToolErrorf(logger, "workspace '%s' not found in org '%s'", workspaceName, terraformOrgName)
	}

	// Get the current state version with outputs included
	sv, err := tfeClient.StateVersions.ReadCurrentWithOptions(ctx, workspace.ID, &tfe.StateVersionCurrentOptions{
		Include: []tfe.StateVersionIncludeOpt{tfe.SVoutputs},
	})
	if err != nil {
		return ToolErrorf(logger, "no state found for workspace '%s' — the workspace may not have been applied yet", workspaceName)
	}

	var sb strings.Builder

	// Header
	sb.WriteString(fmt.Sprintf("# Current State for Workspace: %s/%s\n\n", terraformOrgName, workspaceName))

	// State version metadata
	sb.WriteString("## State Version Metadata\n\n")
	sb.WriteString(fmt.Sprintf("**State Version ID:** %s\n", sv.ID))
	sb.WriteString(fmt.Sprintf("**Serial:** %d\n", sv.Serial))
	sb.WriteString(fmt.Sprintf("**Status:** %s\n", sv.Status))
	sb.WriteString(fmt.Sprintf("**Created At:** %s\n", sv.CreatedAt.Format("2006-01-02 15:04:05 UTC")))

	if sv.ResourcesProcessed {
		sb.WriteString("**Resources Processed:** yes\n")
	} else {
		sb.WriteString("**Resources Processed:** no (some fields may still be populating)\n")
	}

	if sv.TerraformVersion != "" {
		sb.WriteString(fmt.Sprintf("**Terraform Version:** %s\n", sv.TerraformVersion))
	}

	if sv.VCSCommitSHA != "" {
		sb.WriteString(fmt.Sprintf("**VCS Commit SHA:** %s\n", sv.VCSCommitSHA))
	}
	if sv.VCSCommitURL != "" {
		sb.WriteString(fmt.Sprintf("**VCS Commit URL:** %s\n", sv.VCSCommitURL))
	}

	// Resource summary
	if len(sv.Resources) > 0 {
		sb.WriteString("\n## Managed Resources\n\n")
		sb.WriteString("| Type | Name | Module | Provider | Count |\n")
		sb.WriteString("|------|------|--------|----------|-------|\n")
		for _, r := range sv.Resources {
			module := r.Module
			if module == "" {
				module = "(root)"
			}
			sb.WriteString(fmt.Sprintf("| %s | %s | %s | %s | %d |\n", r.Type, r.Name, module, r.Provider, r.Count))
		}
	}

	// Outputs
	if len(sv.Outputs) > 0 {
		sb.WriteString("\n## State Outputs\n\n")
		for _, output := range sv.Outputs {
			if output.Sensitive {
				sb.WriteString(fmt.Sprintf("- **%s** (sensitive, type: %s): `<sensitive>`\n", output.Name, output.Type))
			} else {
				valueStr := formatOutputValue(output.Value)
				sb.WriteString(fmt.Sprintf("- **%s** (type: %s): %s\n", output.Name, output.Type, valueStr))
			}
		}
	} else {
		// Outputs may not have been included in the relation; try listing them separately
		outputs, err := tfeClient.StateVersions.ListOutputs(ctx, sv.ID, &tfe.StateVersionOutputsListOptions{})
		if err == nil && outputs != nil && len(outputs.Items) > 0 {
			sb.WriteString("\n## State Outputs\n\n")
			for _, output := range outputs.Items {
				if output.Sensitive {
					sb.WriteString(fmt.Sprintf("- **%s** (sensitive, type: %s): `<sensitive>`\n", output.Name, output.Type))
				} else {
					valueStr := formatOutputValue(output.Value)
					sb.WriteString(fmt.Sprintf("- **%s** (type: %s): %s\n", output.Name, output.Type, valueStr))
				}
			}
		} else {
			sb.WriteString("\n## State Outputs\n\nNo outputs defined.\n")
		}
	}

	// Optionally download and include the full JSON state
	if includeFullState && sv.JSONDownloadURL != "" {
		stateBytes, err := tfeClient.StateVersions.Download(ctx, sv.JSONDownloadURL)
		if err != nil {
			logger.WithError(err).Warn("Could not download full JSON state")
			sb.WriteString("\n> **Note:** Could not download the full JSON state representation.\n")
		} else {
			// Pretty-print the JSON
			var prettyJSON map[string]interface{}
			if err := json.Unmarshal(stateBytes, &prettyJSON); err == nil {
				formatted, err := json.MarshalIndent(prettyJSON, "", "  ")
				if err == nil {
					stateContent := string(formatted)

					sb.WriteString("\n## Full JSON State\n\n")
					sb.WriteString("```json\n")
					sb.WriteString(stateContent)
					if !strings.HasSuffix(stateContent, "\n") {
						sb.WriteString("\n")
					}
					sb.WriteString("```\n")
				}
			}
		}
	}

	return mcp.NewToolResultText(sb.String()), nil
}

// formatOutputValue converts an output value to a readable string representation.
func formatOutputValue(value interface{}) string {
	if value == nil {
		return "`null`"
	}

	switch v := value.(type) {
	case string:
		return fmt.Sprintf("`%s`", v)
	case float64:
		if v == float64(int64(v)) {
			return fmt.Sprintf("`%d`", int64(v))
		}
		return fmt.Sprintf("`%g`", v)
	case bool:
		return fmt.Sprintf("`%t`", v)
	default:
		// For complex types (maps, lists), marshal to JSON
		jsonBytes, err := json.Marshal(v)
		if err != nil {
			return fmt.Sprintf("`%v`", v)
		}
		return fmt.Sprintf("```json\n%s\n```", string(jsonBytes))
	}
}
