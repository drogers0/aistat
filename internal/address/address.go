// Package address formats and matches human-typable Claude account selectors.
package address

import (
	"regexp"
	"strings"
	"unicode"

	"github.com/drogers0/aistat/v2/internal/accounts"
)

var (
	hexSuffix  = regexp.MustCompile(`(?i)-([0-9a-f]{8,})$`)
	uuidPrefix = regexp.MustCompile(`(?i)^[0-9a-f-]{8,}$`)
)

// AddressFor returns account's canonical printable Claude selector, if it has
// a canonical organization identity.
func AddressFor(stored []accounts.Account, account accounts.Account) string {
	if account.OrganizationUUID == "" {
		return ""
	}
	if personalPath(account) {
		return account.Email + "/personal-" + shortID(account.UUID, personalIDs(stored))
	}
	return account.Email + "/" + organizationBase(account) + "-" + shortID(account.OrganizationUUID, organizationIDs(stored))
}

// AccountLabel returns the canonical selector when one exists, or the legacy
// account display label.
func AccountLabel(stored []accounts.Account, account accounts.Account) string {
	if address := AddressFor(stored, account); address != "" {
		return address
	}
	return account.Email + " (uuid " + account.UUID + ")"
}

// Match returns every account selected by argument. Callers decide whether the
// result is unambiguous.
func Match(stored []accounts.Account, argument string) []accounts.Account {
	arg := strings.ToLower(argument)
	if strings.Contains(arg, "/") {
		email, _, _ := strings.Cut(arg, "/")
		part := arg[strings.LastIndex(arg, "/")+1:]
		if part == "personal" {
			return filter(stored, personalPath)
		}
		if strings.HasPrefix(part, "personal-") && validHexPrefix(strings.TrimPrefix(part, "personal-")) {
			prefix := strings.TrimPrefix(part, "personal-")
			return filter(stored, func(a accounts.Account) bool {
				return personalPath(a) && strings.HasPrefix(compactID(a.UUID), prefix)
			})
		}
		if match := hexSuffix.FindStringSubmatch(part); match != nil {
			prefix := strings.ToLower(match[1])
			matches := filter(stored, func(a accounts.Account) bool {
				return organizationPath(a) && strings.HasPrefix(compactID(a.OrganizationUUID), prefix)
			})
			// Organization UUID prefixes are the primary identity. A duplicate
			// organization UUID can only occur under different account identities;
			// in that tie, the printed email distinguishes those contexts.
			if len(matches) > 1 && email != "" {
				return filter(matches, func(a accounts.Account) bool {
					return strings.EqualFold(a.Email, email)
				})
			}
			return matches
		}
		return filter(stored, func(a accounts.Account) bool {
			return organizationPath(a) && strings.ToLower(a.Email+"/"+organizationBase(a)) == arg
		})
	}
	if arg == "personal" {
		return filter(stored, personalPath)
	}
	if matches := filter(stored, func(a accounts.Account) bool {
		return organizationPath(a) && organizationBase(a) == arg
	}); len(matches) != 0 {
		return matches
	}
	if uuidPrefix.MatchString(argument) {
		return filter(stored, func(a accounts.Account) bool {
			return strings.HasPrefix(strings.ToLower(a.UUID), arg)
		})
	}
	return filter(stored, func(a accounts.Account) bool {
		return strings.Contains(strings.ToLower(a.Email), arg)
	})
}

func filter(stored []accounts.Account, keep func(accounts.Account) bool) []accounts.Account {
	var matched []accounts.Account
	for _, account := range stored {
		if keep(account) {
			matched = append(matched, account)
		}
	}
	return matched
}

func personalPath(account accounts.Account) bool {
	return account.OrganizationUUID == accounts.PersonalOrganizationUUID ||
		account.OrganizationType == "claude_max" || account.OrganizationType == "claude_pro"
}

func organizationPath(account accounts.Account) bool {
	return account.OrganizationUUID != "" && !personalPath(account)
}

func personalIDs(stored []accounts.Account) []string {
	var ids []string
	for _, account := range stored {
		if personalPath(account) {
			ids = append(ids, account.UUID)
		}
	}
	return ids
}

func organizationIDs(stored []accounts.Account) []string {
	var ids []string
	for _, account := range stored {
		if organizationPath(account) {
			ids = append(ids, account.OrganizationUUID)
		}
	}
	return ids
}

func shortID(id string, ids []string) string {
	compact := compactID(id)
	group := make(map[string]struct{})
	for _, other := range ids {
		other = compactID(other)
		if len(other) >= 8 && len(compact) >= 8 && other[:8] == compact[:8] {
			group[other] = struct{}{}
		}
	}
	width := 8
	if len(group) > 1 {
		for left := range group {
			for right := range group {
				if left == right {
					continue
				}
				if common := commonPrefix(left, right) + 1; common > width {
					width = common
				}
			}
		}
	}
	if width > len(compact) {
		width = len(compact)
	}
	return compact[:width]
}

func compactID(id string) string { return strings.ToLower(strings.ReplaceAll(id, "-", "")) }

func commonPrefix(left, right string) int {
	max := len(left)
	if len(right) < max {
		max = len(right)
	}
	for i := 0; i < max; i++ {
		if left[i] != right[i] {
			return i
		}
	}
	return max
}

func validHexPrefix(s string) bool {
	return len(s) >= 8 && strings.Trim(s, "0123456789abcdef") == ""
}

func organizationBase(account accounts.Account) string {
	base := slug(account.OrganizationName)
	if base == "" || base == "personal" {
		return "organization"
	}
	return base
}

func slug(input string) string {
	var b strings.Builder
	separator := false
	for _, r := range strings.ToLower(input) {
		if r <= unicode.MaxASCII && ((r >= 'a' && r <= 'z') || (r >= '0' && r <= '9')) {
			if separator && b.Len() > 0 {
				b.WriteByte('-')
			}
			b.WriteRune(r)
			separator = false
		} else if b.Len() > 0 {
			separator = true
		}
	}
	return b.String()
}
