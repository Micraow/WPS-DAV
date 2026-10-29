package wps

import (
	"context"
	"encoding/json"
	"fmt"
)

// Mkdir creates a folder and returns its file id.
func (c *Client) Mkdir(ctx context.Context, groupID, parentID int64, name string) (int64, error) {
	body := map[string]any{
		"groupid":  groupID,
		"parentid": parentID,
		"name":     name,
	}
	data, _, err := c.doJSON(ctx, "POST", c.driveURL("/api/v5/files/folder"), body)
	if err != nil {
		return 0, err
	}
	var r struct {
		apiResult
		ID int64 `json:"id"`
	}
	if err := json.Unmarshal(data, &r); err != nil {
		return 0, fmt.Errorf("mkdir: %w", err)
	}
	if r.Result != "" && r.Result != "ok" {
		return 0, fmt.Errorf("mkdir: %s", r.Msg)
	}
	if r.ID == 0 {
		return 0, fmt.Errorf("mkdir: response missing id")
	}
	return r.ID, nil
}

// Rename renames a file inside its current folder.
func (c *Client) Rename(ctx context.Context, groupID, fileID int64, newName string) error {
	u := c.driveURL(fmt.Sprintf("/api/v3/groups/%d/files/%d", groupID, fileID))
	_, err := c.doJSONResult(ctx, "PUT", u, map[string]any{"fname": newName})
	return err
}

// Move moves a file/folder to another folder (possibly another group).
func (c *Client) Move(ctx context.Context, srcGroupID, fileID, targetGroupID, targetParentID int64) error {
	u := c.driveURL(fmt.Sprintf("/api/v3/groups/%d/files/batch/move", srcGroupID))
	_, err := c.doJSONResult(ctx, "POST", u, map[string]any{
		"fileids":         []int64{fileID},
		"target_groupid":  targetGroupID,
		"target_parentid": targetParentID,
	})
	return err
}

// Copy copies a file/folder into another folder.
func (c *Client) Copy(ctx context.Context, srcGroupID, fileID, targetGroupID, targetParentID int64) error {
	u := c.driveURL(fmt.Sprintf("/api/v3/groups/%d/files/batch/copy", srcGroupID))
	_, err := c.doJSONResult(ctx, "POST", u, map[string]any{
		"fileids":               []int64{fileID},
		"groupid":               srcGroupID,
		"target_groupid":        targetGroupID,
		"target_parentid":       targetParentID,
		"duplicated_name_model": 1,
	})
	return err
}

// Delete removes a file or folder (it lands in the recycle bin).
func (c *Client) Delete(ctx context.Context, groupID, fileID int64) error {
	u := c.driveURL(fmt.Sprintf("/api/v3/groups/%d/files/batch/delete", groupID))
	_, err := c.doJSONResult(ctx, "POST", u, map[string]any{"fileids": []int64{fileID}})
	return err
}
