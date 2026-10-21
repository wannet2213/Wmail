package models

import "time"

type Email struct {
	ID             int64        `json:"id"`
	MailboxID      int64        `json:"mailboxId"`
	Provider       string       `json:"provider,omitempty"`
	MessageID      string       `json:"messageId"`
	InReplyTo      string       `json:"inReplyTo,omitempty"`
	ThreadID       string       `json:"threadId,omitempty"`
	FromName       string       `json:"fromName"`
	FromEmail      string       `json:"fromEmail"`
	ToRecipients   []string     `json:"toRecipients"`
	CcRecipients   []string     `json:"ccRecipients,omitempty"`
	Subject        string       `json:"subject"`
	Snippet        string       `json:"snippet"`
	BodyText       string       `json:"bodyText"`
	BodyHTML       string       `json:"bodyHtml"`
	IsUnread       bool         `json:"isUnread"`
	IsStarred      bool         `json:"isStarred"`
	IsArchived     bool         `json:"isArchived"`
	IsDeleted      bool         `json:"isDeleted"`
	Folder         string       `json:"folder"`             // inbox, sent, drafts, trash, spam
	Category       string       `json:"category,omitempty"` // social, updates, promotions, forums
	Label          string       `json:"label,omitempty"`    // Work, Finance, Travel
	HasAttachments bool         `json:"hasAttachments"`
	Attachments    []Attachment `json:"attachments,omitempty"`
	ThreadCount    int          `json:"threadCount,omitempty"` // list rows only: messages in the conversation
	InInbox        bool         `json:"inInbox,omitempty"`     // list rows only: conversation shows in Kotak masuk
	CreatedAt      time.Time    `json:"createdAt"`
	UpdatedAt      time.Time    `json:"updatedAt"`
}

type Attachment struct {
	ID          string    `json:"id"`
	EmailID     int64     `json:"emailId"`
	Filename    string    `json:"filename"`
	ContentType string    `json:"contentType"`
	SizeBytes   int64     `json:"sizeBytes"`
	FilePath    string    `json:"-"`
	CreatedAt   time.Time `json:"createdAt"`
}

type SendRequest struct {
	To        string `json:"to"`
	Cc        string `json:"cc,omitempty"`
	Subject   string `json:"subject"`
	Body      string `json:"body"`
	IsHTML    bool   `json:"isHtml"`
	InReplyTo string `json:"inReplyTo,omitempty"`
	From      int64  `json:"from"` // mailbox id
}

type Stats struct {
	InboxUnread  int `json:"inboxUnread"`
	StarredCount int `json:"starredCount"`
	SentCount    int `json:"sentCount"`
	DraftsCount  int `json:"draftsCount"`
	TrashCount   int `json:"trashCount"`
}

type UpdateStatusRequest struct {
	IsUnread   *bool   `json:"isUnread,omitempty"`
	IsStarred  *bool   `json:"isStarred,omitempty"`
	IsArchived *bool   `json:"isArchived,omitempty"`
	IsDeleted  *bool   `json:"isDeleted,omitempty"`
	Folder     *string `json:"folder,omitempty"`
	Label      *string `json:"label,omitempty"`
}

type User struct {
	ID        int64     `json:"id"`
	Username  string    `json:"username"`
	IsAdmin   bool      `json:"isAdmin"`
	CreatedAt time.Time `json:"createdAt"`
}

type Mailbox struct {
	ID          int64  `json:"id"`
	Address     string `json:"address"`
	DisplayName string `json:"displayName"`
	CanSend     bool   `json:"canSend"` // per-user view; admins always true
}

type Grant struct {
	UserID    int64 `json:"userId"`
	MailboxID int64 `json:"mailboxId"`
	CanSend   bool  `json:"canSend"`
}
