package archive

import (
	"context"
	"strings"
)

// Directory resolves JIDs to human-readable names using everything the
// archive knows: address book names, push names, group subjects and chat names.
type Directory struct {
	Self      string
	contacts  map[string]Contact
	groups    map[string]Group
	chatNames map[string]string
	lids      map[string]string
	metas     map[string]ContactMeta
}

// Directory loads a name directory snapshot.
func (s *Store) Directory(ctx context.Context) (*Directory, error) {
	d := &Directory{
		contacts:  map[string]Contact{},
		groups:    map[string]Group{},
		chatNames: map[string]string{},
	}
	var err error
	if d.Self, err = s.Meta(ctx, MetaAccount); err != nil {
		return nil, err
	}
	contacts, err := s.Contacts(ctx)
	if err != nil {
		return nil, err
	}
	for _, c := range contacts {
		d.contacts[c.JID] = c
	}
	groups, err := s.Groups(ctx)
	if err != nil {
		return nil, err
	}
	for _, g := range groups {
		d.groups[g.JID] = g
	}
	rows, err := s.read.QueryContext(ctx, `SELECT jid, name FROM chats WHERE name IS NOT NULL AND name <> ''`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var jid, name string
		if err := rows.Scan(&jid, &name); err != nil {
			return nil, err
		}
		d.chatNames[jid] = name
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if d.lids, err = s.LIDMappings(ctx); err != nil {
		return nil, err
	}
	if d.metas, err = s.ContactMetas(ctx); err != nil {
		return nil, err
	}
	return d, nil
}

// Canonical maps a LID to its phone JID when known.
func (d *Directory) Canonical(jid string) string {
	if pn, ok := d.lids[jid]; ok {
		return pn
	}
	return jid
}

// IsSelf reports whether jid is the archived account.
func (d *Directory) IsSelf(jid string) bool {
	return jid != "" && d.Canonical(jid) == d.Self
}

// Contact returns the stored contact for jid.
func (d *Directory) Contact(jid string) (Contact, bool) {
	c, ok := d.contacts[d.Canonical(jid)]
	return c, ok
}

// Meta returns the user annotation for jid, if any.
func (d *Directory) Meta(jid string) (ContactMeta, bool) {
	m, ok := d.metas[d.Canonical(jid)]
	return m, ok
}

// Group returns stored group metadata.
func (d *Directory) Group(jid string) (Group, bool) {
	g, ok := d.groups[jid]
	return g, ok
}

// Contacts returns every stored contact.
func (d *Directory) Contacts() []Contact {
	out := make([]Contact, 0, len(d.contacts))
	for _, c := range d.contacts {
		out = append(out, c)
	}
	return out
}

// Name returns the best display name for a user, group or other chat JID,
// preferring a user-set alias.
func (d *Directory) Name(jid string) string { return d.name(jid, true) }

// WhatsAppName is the name WhatsApp knows, ignoring a user-set alias.
func (d *Directory) WhatsAppName(jid string) string { return d.name(jid, false) }

func (d *Directory) name(jid string, alias bool) string {
	if jid == "" {
		return "unknown"
	}
	jid = d.Canonical(jid)
	if m, ok := d.metas[jid]; alias && ok && m.Alias != "" {
		return m.Alias
	}
	user, server, _ := strings.Cut(jid, "@")
	switch server {
	case "g.us":
		if g, ok := d.groups[jid]; ok && g.Name != "" {
			return g.Name
		}
		if n := d.chatNames[jid]; n != "" {
			return n
		}
		return "group " + user
	case "broadcast":
		if user == "status" {
			return "Status updates"
		}
		if n := d.chatNames[jid]; n != "" {
			return n
		}
		return "broadcast list " + user
	case "newsletter":
		if n := d.chatNames[jid]; n != "" {
			return n
		}
		return "channel " + user
	}
	if c, ok := d.contacts[jid]; ok {
		for _, n := range []string{c.FullName, c.FirstName, c.BusinessName, c.PushName} {
			if n != "" {
				return n
			}
		}
	}
	if n := d.chatNames[jid]; n != "" {
		return n
	}
	return Phone(jid)
}

// Label is Name plus the phone number for users, e.g. "Anna (+48600000101)".
func (d *Directory) Label(jid string) string {
	name := d.Name(jid)
	phone := Phone(d.Canonical(jid))
	if strings.HasPrefix(phone, "+") && phone != name {
		return name + " (" + phone + ")"
	}
	return name
}

// Phone formats a user JID as a phone number, or a readable placeholder for LIDs.
func Phone(jid string) string {
	user, server, _ := strings.Cut(jid, "@")
	user, _, _ = strings.Cut(user, ":")
	switch server {
	case "s.whatsapp.net", "c.us":
		return "+" + user
	case "lid":
		return "hidden number (lid " + user + ")"
	}
	return jid
}
