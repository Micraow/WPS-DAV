// Package wps implements the cookie-authenticated WPS netdisk API.
package wps

// apiResult is the envelope returned by mutating endpoints.
type apiResult struct {
	Result string `json:"result"`
	Msg    string `json:"msg"`
}

// LoginState is the islogin response.
type LoginState struct {
	AccountNum       int    `json:"account_num"`
	CompanyID        int64  `json:"companyid"`
	CurrentCompanyID int64  `json:"current_companyid"`
	IsCompanyAccount bool   `json:"is_company_account"`
	IsPlus           bool   `json:"is_plus"`
	LoginMode        string `json:"loginmode"`
	UserID           int64  `json:"userid"`
	Result           string `json:"result"`
	Msg              string `json:"msg"`
}

// Group is one cloud drive container (personal "my docs" or a team library).
type Group struct {
	CompanyID int64  `json:"company_id"`
	GroupID   int64  `json:"group_id"`
	Name      string `json:"name"`
	Type      string `json:"type"`
}

// FileInfo is one entry from the file listing endpoint.
type FileInfo struct {
	GroupID   int64  `json:"groupid"`
	ParentID  int64  `json:"parentid"`
	Name      string `json:"fname"`
	Size      int64  `json:"fsize"`
	Type      string `json:"ftype"`
	Ctime     int64  `json:"ctime"`
	Mtime     int64  `json:"mtime"`
	ID        int64  `json:"id"`
	Deleted   bool   `json:"deleted"`
	FilePerms perms  `json:"file_perms_acl"`
}

type perms struct {
	Download int `json:"download"`
}

// IsDir reports whether the entry is a folder.
func (f FileInfo) IsDir() bool { return f.Type == "folder" }

// filesResp is the paginated listing response.
type filesResp struct {
	Files      []FileInfo `json:"files"`
	NextOffset int        `json:"next_offset"`
	Result     string     `json:"result"`
	Msg        string     `json:"msg"`
}

// personalGroupsResp is the personal /api/v3/groups response.
type personalGroupsResp struct {
	Result string `json:"result"`
	Msg    string `json:"msg"`
	Groups []struct {
		ID   int64  `json:"id"`
		Name string `json:"name"`
	} `json:"groups"`
}

// businessGroupsResp is the 365 private groups response.
type businessGroupsResp struct {
	Groups []Group `json:"groups"`
}

// specialGroupResp is the personal /api/v3/groups/special response.
type specialGroupResp struct {
	ID     int64  `json:"id"`
	Name   string `json:"name"`
	Result string `json:"result"`
	Msg    string `json:"msg"`
}

// downloadResp is the download-link response.
type downloadResp struct {
	URL    string `json:"url"`
	Result string `json:"result"`
	Msg    string `json:"msg"`
}

// spacesResp is the personal quota response.
type spacesResp struct {
	ID     int64  `json:"id"`
	Name   string `json:"name"`
	Result string `json:"result"`
	Total  int64  `json:"total"`
	Used   int64  `json:"used"`
}

// serviceSpaceResp is the business quota response.
type serviceSpaceResp struct {
	Info []struct {
		ID         int64 `json:"id"`
		SpaceTotal int64 `json:"space_total"`
		SpaceUsed  int64 `json:"space_used"`
	} `json:"info"`
}

// uploadPlan is the create_update response describing how to push bytes.
type uploadPlan struct {
	apiResult
	Method  string `json:"method"`
	URL     string `json:"url"`
	Store   string `json:"store"`
	Request struct {
		Headers  map[string]string `json:"headers"`
		FormData map[string]string `json:"formData"`
	} `json:"request"`
	Response struct {
		ExpectCode []int  `json:"expect_code"`
		ArgsETag   string `json:"args_etag"`
		ArgsKey    string `json:"args_key"`
	} `json:"response"`
}

// uploadPutResp carries the store-side result of the byte upload.
type uploadPutResp struct {
	NewFilename string `json:"newfilename"`
	Sha1        string `json:"sha1"`
	MD5         string `json:"md5"`
}

// commitResp is the commit_upload response.
type commitResp struct {
	apiResult
	ID   int64  `json:"id"`
	Size int64  `json:"fsize"`
	Ver  int64  `json:"fver"`
	Sha  string `json:"fsha"`
}
