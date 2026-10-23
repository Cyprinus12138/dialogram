package mcp

import (
	"strings"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/Cyprinus12138/dialogram/tool"
)

// Keys and type tags of the mapped result; see the package doc.
const (
	keyText        = "text"
	keyAttachments = "attachments"

	partImage        = "image"
	partAudio        = "audio"
	partResource     = "resource"
	partResourceLink = "resource_link"
	partUnknown      = "unknown"
)

// mapResult turns a tools/call result into the value Fn returns.
func mapResult(res *sdk.CallToolResult) any {
	if res == nil {
		return map[string]any{keyText: ""}
	}
	if res.IsError {
		msg := joinText(res.Content)
		if msg == "" {
			msg = "the tool reported an error without a message"
		}
		return tool.Soft(ErrorCodeToolError, msg)
	}
	if res.StructuredContent != nil {
		return res.StructuredContent
	}
	return contentObject(res.Content)
}

// joinText concatenates the text parts with newlines, skipping the rest.
func joinText(parts []sdk.Content) string {
	var texts []string
	for _, p := range parts {
		if t, ok := p.(*sdk.TextContent); ok && t != nil {
			texts = append(texts, t.Text)
		}
	}
	return strings.Join(texts, "\n")
}

// contentObject builds {"text": …} and, when non-text parts exist,
// {"text": …, "attachments": […]}. Binary never enters the object.
func contentObject(parts []sdk.Content) map[string]any {
	out := map[string]any{keyText: joinText(parts)}
	var attachments []map[string]any
	for _, p := range parts {
		switch v := p.(type) {
		case *sdk.TextContent:
			// already joined
		case *sdk.ImageContent:
			attachments = append(attachments, map[string]any{
				"type": partImage, "media_type": v.MIMEType, "omitted": true,
			})
		case *sdk.AudioContent:
			attachments = append(attachments, map[string]any{
				"type": partAudio, "media_type": v.MIMEType, "omitted": true,
			})
		case *sdk.EmbeddedResource:
			attachments = append(attachments, embeddedResource(v))
		case *sdk.ResourceLink:
			a := map[string]any{"type": partResourceLink, "uri": v.URI}
			if v.Name != "" {
				a["name"] = v.Name
			}
			if v.Title != "" {
				a["title"] = v.Title
			}
			if v.Description != "" {
				a["description"] = v.Description
			}
			if v.MIMEType != "" {
				a["media_type"] = v.MIMEType
			}
			attachments = append(attachments, a)
		default:
			attachments = append(attachments, map[string]any{"type": partUnknown, "omitted": true})
		}
	}
	if len(attachments) > 0 {
		out[keyAttachments] = attachments
	}
	return out
}

// embeddedResource keeps a text resource's text (it is what the server
// meant the model to read) and marks a blob resource omitted.
func embeddedResource(v *sdk.EmbeddedResource) map[string]any {
	a := map[string]any{"type": partResource}
	if v == nil || v.Resource == nil {
		a["omitted"] = true
		return a
	}
	r := v.Resource
	a["uri"] = r.URI
	if r.MIMEType != "" {
		a["media_type"] = r.MIMEType
	}
	if r.Text != "" || len(r.Blob) == 0 {
		a[keyText] = r.Text
	} else {
		a["omitted"] = true
	}
	return a
}
