package address

import (
	"reflect"
	"testing"

	"github.com/drogers0/aistat/v2/internal/accounts"
)

func selectorAccount(uuid, email, organizationUUID, organizationType string) accounts.Account {
	return accounts.Account{
		UUID:             uuid,
		Email:            email,
		OrganizationUUID: organizationUUID,
		OrganizationType: organizationType,
	}
}

func organization(uuid, email, organizationUUID, name, organizationType string) accounts.Account {
	return accounts.Account{
		UUID:             uuid,
		Email:            email,
		OrganizationUUID: organizationUUID,
		OrganizationName: name,
		OrganizationType: organizationType,
	}
}

func matchKeys(accounts []accounts.Account) []string {
	keys := make([]string, len(accounts))
	for i, account := range accounts {
		keys[i] = account.Key()
	}
	return keys
}

func TestAddressFor(t *testing.T) {
	tests := []struct {
		name string
		run  func(t *testing.T)
	}{
		{"literal canonical selectors round trip", func(t *testing.T) {
			max := organization("9f2a41c7-3b5d-4e7f-9a1c-2d4e6f8a0b1c", "me@example.com", "7d3c58e9-6a2b-4f81-b771-1c9e5d3a7042", "me@example.com's Organization", "claude_max")
			pro := organization("8f2a41c7-3b5d-4e7f-9a1c-2d4e6f8a0b1c", "pro@example.com", "6d3c58e9-6a2b-4f81-b771-1c9e5d3a7042", "Pro Organization", "claude_pro")
			team := organization("60000000-0000-4000-8000-000000000006", "team@example.com", "44444444-4444-4444-8444-444444444444", "Acme Corp", "claude_team")
			punctuation := organization("61000000-0000-4000-8000-000000000006", "punctuation@example.com", "55555555-5555-4555-8555-555555555555", "R&D / West Team!", "claude_team")
			stored := []accounts.Account{max, pro, team, punctuation}
			for _, test := range []struct {
				account accounts.Account
				want    string
				key     string
			}{
				{max, "me@example.com/personal-9f2a41c7", "9f2a41c7-3b5d-4e7f-9a1c-2d4e6f8a0b1c_7d3c58e9-6a2b-4f81-b771-1c9e5d3a7042"},
				{pro, "pro@example.com/personal-8f2a41c7", "8f2a41c7-3b5d-4e7f-9a1c-2d4e6f8a0b1c_6d3c58e9-6a2b-4f81-b771-1c9e5d3a7042"},
				{team, "team@example.com/acme-corp-44444444", "60000000-0000-4000-8000-000000000006_44444444-4444-4444-8444-444444444444"},
				{punctuation, "punctuation@example.com/r-d-west-team-55555555", "61000000-0000-4000-8000-000000000006_55555555-5555-4555-8555-555555555555"},
			} {
				if got := AddressFor(stored, test.account); got != test.want {
					t.Errorf("AddressFor(%s) = %q, want %q", test.account.UUID, got, test.want)
				}
				if got, want := matchKeys(Match(stored, test.want)), []string{test.key}; !reflect.DeepEqual(got, want) {
					t.Errorf("Match(%q) = %v, want %v", test.want, got, want)
				}
			}
		}},
		{"collision groups use their exact maximum width", func(t *testing.T) {
			orgA := organization("10000000-0000-4000-8000-000000000001", "org-a@example.com", "abcdef01-2345-4aaa-8111-111111111111", "Acme Corp", "claude_team")
			orgB := organization("20000000-0000-4000-8000-000000000002", "org-b@example.com", "abcdef01-2345-4bbb-8222-222222222222", "Acme Corp", "claude_team")
			orgC := organization("30000000-0000-4000-8000-000000000003", "org-c@example.com", "abcdef01-9999-4ccc-8333-333333333333", "Acme Corp", "claude_team")
			personalA := organization("abcdef01-2345-4aaa-8111-111111111111", "person-a@example.com", "61111111-1111-4111-8111-111111111111", "Personal A", "claude_max")
			personalB := organization("abcdef01-2345-4bbb-8222-222222222222", "person-b@example.com", "72222222-2222-4222-8222-222222222222", "Personal B", "claude_pro")
			personalC := organization("abcdef01-9999-4ccc-8333-333333333333", "person-c@example.com", "83333333-3333-4333-8333-333333333333", "Personal C", "claude_max")
			stored := []accounts.Account{orgA, orgB, orgC, personalA, personalB, personalC}
			for _, test := range []struct {
				account accounts.Account
				want    string
				key     string
			}{
				{orgA, "org-a@example.com/acme-corp-abcdef0123454a", "10000000-0000-4000-8000-000000000001_abcdef01-2345-4aaa-8111-111111111111"},
				{orgB, "org-b@example.com/acme-corp-abcdef0123454b", "20000000-0000-4000-8000-000000000002_abcdef01-2345-4bbb-8222-222222222222"},
				{orgC, "org-c@example.com/acme-corp-abcdef0199994c", "30000000-0000-4000-8000-000000000003_abcdef01-9999-4ccc-8333-333333333333"},
				{personalA, "person-a@example.com/personal-abcdef0123454a", "abcdef01-2345-4aaa-8111-111111111111_61111111-1111-4111-8111-111111111111"},
				{personalB, "person-b@example.com/personal-abcdef0123454b", "abcdef01-2345-4bbb-8222-222222222222_72222222-2222-4222-8222-222222222222"},
				{personalC, "person-c@example.com/personal-abcdef0199994c", "abcdef01-9999-4ccc-8333-333333333333_83333333-3333-4333-8333-333333333333"},
			} {
				if got := AddressFor(stored, test.account); got != test.want {
					t.Errorf("AddressFor(%s) = %q, want %q", test.account.UUID, got, test.want)
				}
				if got, want := matchKeys(Match(stored, test.want)), []string{test.key}; !reflect.DeepEqual(got, want) {
					t.Errorf("Match(%q) = %v, want %v", test.want, got, want)
				}
			}
		}},
		{"reserved and fallback organization names", func(t *testing.T) {
			reserved := organization("30000000-0000-4000-8000-000000000003", "team@example.com", "11111111-1111-4111-8111-111111111111", "personal", "claude_team")
			empty := organization("40000000-0000-4000-8000-000000000004", "empty@example.com", "22222222-2222-4222-8222-222222222222", "", "")
			for _, test := range []struct {
				account accounts.Account
				want    string
			}{
				{reserved, "team@example.com/organization-11111111"},
				{empty, "empty@example.com/organization-22222222"},
			} {
				if got := AddressFor([]accounts.Account{test.account}, test.account); got != test.want {
					t.Errorf("AddressFor(%s) = %q, want %q", test.account.UUID, got, test.want)
				}
			}
		}},
		{"legacy and codex have no address", func(t *testing.T) {
			legacy := selectorAccount("legacy", "legacy@example.com", "", "")
			if got := AddressFor([]accounts.Account{legacy}, legacy); got != "" {
				t.Errorf("AddressFor legacy = %q, want empty", got)
			}
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, tt.run)
	}
}

func TestAccountLabel(t *testing.T) {
	tests := []struct {
		name string
		run  func(t *testing.T)
	}{
		{"canonical address", func(t *testing.T) {
			account := organization("50000000-0000-4000-8000-000000000005", "team@example.com", "33333333-3333-4333-8333-333333333333", "Team", "claude_team")
			if got, want := AccountLabel([]accounts.Account{account}, account), "team@example.com/team-33333333"; got != want {
				t.Errorf("AccountLabel = %q, want %q", got, want)
			}
		}},
		{"legacy fallback", func(t *testing.T) {
			account := selectorAccount("legacy-id", "legacy@example.com", "", "")
			if got, want := AccountLabel([]accounts.Account{account}, account), "legacy@example.com (uuid legacy-id)"; got != want {
				t.Errorf("AccountLabel = %q, want %q", got, want)
			}
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, tt.run)
	}
}

func TestMatch(t *testing.T) {
	tests := []struct {
		name string
		run  func(t *testing.T)
	}{
		{"canonical forms select their keys case insensitively", func(t *testing.T) {
			team := organization("60000000-0000-4000-8000-000000000006", "team@example.com", "44444444-4444-4444-8444-444444444444", "Acme Corp", "claude_team")
			pro := organization("70000000-0000-4000-8000-000000000007", "pro@example.com", "55555555-5555-4555-8555-555555555555", "Pro", "claude_pro")
			stored := []accounts.Account{team, pro}
			for _, test := range []struct {
				argument string
				key      string
			}{
				{"TEAM@EXAMPLE.COM/SCOPE-LAB-44444444", "60000000-0000-4000-8000-000000000006_44444444-4444-4444-8444-444444444444"},
				{"PRO@EXAMPLE.COM/PERSONAL-70000000", "70000000-0000-4000-8000-000000000007_55555555-5555-4555-8555-555555555555"},
			} {
				if got, want := matchKeys(Match(stored, test.argument)), []string{test.key}; !reflect.DeepEqual(got, want) {
					t.Errorf("Match(%q) = %v, want %v", test.argument, got, want)
				}
			}
		}},
		{"clean slash and bare aliases are unique only", func(t *testing.T) {
			one := organization("80000000-0000-4000-8000-000000000008", "one@example.com", "66666666-6666-4666-8666-666666666666", "Acme Corp", "claude_team")
			two := organization("90000000-0000-4000-8000-000000000009", "one@example.com", "77777777-7777-4777-8777-777777777777", "Acme Corp", "claude_team")
			if got := matchKeys(Match([]accounts.Account{one}, "ONE@example.com/acme-corp")); !reflect.DeepEqual(got, []string{"80000000-0000-4000-8000-000000000008_66666666-6666-4666-8666-666666666666"}) {
				t.Errorf("unique clean slash = %v", got)
			}
			for _, argument := range []string{"acme-corp", "one@example.com/acme-corp"} {
				if got := Match([]accounts.Account{one, two}, argument); len(got) != 2 {
					t.Errorf("Match(%q) count = %d, want 2", argument, len(got))
				}
			}
		}},
		{"bare hex shaped slug takes precedence over UUID prefix", func(t *testing.T) {
			organizationAccount := organization("11111111-1111-4111-8111-111111111111", "team@example.com", "22222222-2222-4222-8222-222222222222", "Decaf Beef", "claude_team")
			uuidAccount := organization("decaf-beef-1111-4111-8111-111111111111", "uuid@example.com", "33333333-3333-4333-8333-333333333333", "Other", "claude_team")
			stored := []accounts.Account{organizationAccount, uuidAccount}
			if got, want := matchKeys(Match(stored, "decaf-beef")), []string{"11111111-1111-4111-8111-111111111111_22222222-2222-4222-8222-222222222222"}; !reflect.DeepEqual(got, want) {
				t.Errorf("Match(\"decaf-beef\") = %v, want %v", got, want)
			}
		}},
		{"UUID prefix applies when no organization slug matches", func(t *testing.T) {
			account := organization("decaf-beef-1111-4111-8111-111111111111", "uuid@example.com", "33333333-3333-4333-8333-333333333333", "Other", "claude_team")
			if got, want := matchKeys(Match([]accounts.Account{account}, "decaf-beef")), []string{"decaf-beef-1111-4111-8111-111111111111_33333333-3333-4333-8333-333333333333"}; !reflect.DeepEqual(got, want) {
				t.Errorf("Match(\"decaf-beef\") = %v, want %v", got, want)
			}
		}},
		{"personal aliases are unique only", func(t *testing.T) {
			one := organization("81000000-0000-4000-8000-000000000008", "one@example.com", "76666666-6666-4666-8666-666666666666", "One", "claude_max")
			two := organization("91000000-0000-4000-8000-000000000009", "two@example.com", "87777777-7777-4777-8777-777777777777", "Two", "claude_pro")
			if got := matchKeys(Match([]accounts.Account{one}, "anything@example.com/personal")); !reflect.DeepEqual(got, []string{"81000000-0000-4000-8000-000000000008_76666666-6666-4666-8666-666666666666"}) {
				t.Errorf("unique personal alias = %v", got)
			}
			for _, argument := range []string{"personal", "anything@example.com/personal"} {
				if got := Match([]accounts.Account{one, two}, argument); len(got) != 2 {
					t.Errorf("Match(%q) count = %d, want 2", argument, len(got))
				}
			}
		}},
		{"nil organization sentinels widen personal selectors", func(t *testing.T) {
			left := organization("abcdef01-2345-4aaa-8111-111111111111", "left@example.com", accounts.PersonalOrganizationUUID, "", "")
			right := organization("abcdef01-2345-4bbb-8222-222222222222", "right@example.com", accounts.PersonalOrganizationUUID, "", "")
			stored := []accounts.Account{left, right}
			for _, test := range []struct {
				account     accounts.Account
				selector    string
				matchedKeys []string
			}{
				{left, "left@example.com/personal-abcdef0123454a", []string{"abcdef01-2345-4aaa-8111-111111111111_personal"}},
				{right, "right@example.com/personal-abcdef0123454b", []string{"abcdef01-2345-4bbb-8222-222222222222_personal"}},
			} {
				if got := AddressFor(stored, test.account); got != test.selector {
					t.Errorf("AddressFor(%s) = %q, want %q", test.account.UUID, got, test.selector)
				}
				if got := matchKeys(Match(stored, test.selector)); !reflect.DeepEqual(got, test.matchedKeys) {
					t.Errorf("Match(%q) = %v, want %v", test.selector, got, test.matchedKeys)
				}
			}
		}},
		{"same account personal paths remain deliberately ambiguous", func(t *testing.T) {
			sentinel := organization("a1000000-0000-4000-8000-000000000010", "same@example.com", accounts.PersonalOrganizationUUID, "", "")
			real := organization("a1000000-0000-4000-8000-000000000010", "same@example.com", "98888888-8888-4888-8888-888888888888", "Personal", "claude_max")
			stored := []accounts.Account{sentinel, real}
			selector := "same@example.com/personal-a1000000"
			for _, account := range stored {
				if got := AddressFor(stored, account); got != selector {
					t.Errorf("AddressFor(%s) = %q, want %q", account.OrganizationUUID, got, selector)
				}
			}
			if got, want := matchKeys(Match(stored, selector)), []string{
				"a1000000-0000-4000-8000-000000000010_personal",
				"a1000000-0000-4000-8000-000000000010_98888888-8888-4888-8888-888888888888",
			}; !reflect.DeepEqual(got, want) {
				t.Errorf("Match(%q) = %v, want %v", selector, got, want)
			}
		}},
		{"unknown and empty organization types remain organizations", func(t *testing.T) {
			unknown := organization("a0000000-0000-4000-8000-000000000010", "unknown@example.com", "88888888-8888-4888-8888-888888888888", "Unknown", "raven")
			empty := organization("b0000000-0000-4000-8000-000000000011", "empty@example.com", "99999999-9999-4999-8999-999999999999", "Empty", "")
			stored := []accounts.Account{unknown, empty}
			for _, test := range []struct {
				account     accounts.Account
				selector    string
				matchedKeys []string
			}{
				{unknown, "unknown@example.com/unknown-88888888", []string{"a0000000-0000-4000-8000-000000000010_88888888-8888-4888-8888-888888888888"}},
				{empty, "empty@example.com/empty-99999999", []string{"b0000000-0000-4000-8000-000000000011_99999999-9999-4999-8999-999999999999"}},
			} {
				if got := AddressFor(stored, test.account); got != test.selector {
					t.Errorf("AddressFor(%s) = %q, want %q", test.account.UUID, got, test.selector)
				}
				if got := matchKeys(Match(stored, test.selector)); !reflect.DeepEqual(got, test.matchedKeys) {
					t.Errorf("Match(%q) = %v, want %v", test.selector, got, test.matchedKeys)
				}
			}
		}},
		{"organization collision widens and old short form fails closed", func(t *testing.T) {
			left := organization("c0000000-0000-4000-8000-000000000012", "left@example.com", "abcdefab-1111-4111-8111-111111111111", "Left", "claude_team")
			right := organization("d0000000-0000-4000-8000-000000000013", "right@example.com", "abcdefab-2222-4222-8222-222222222222", "Right", "claude_team")
			stored := []accounts.Account{left, right}
			if got := Match(stored, "/renamed-abcdefab"); len(got) != 2 {
				t.Errorf("old short form count = %d, want 2", len(got))
			}
			for _, test := range []struct {
				selector string
				key      string
			}{
				{"left@example.com/left-abcdefab1", "c0000000-0000-4000-8000-000000000012_abcdefab-1111-4111-8111-111111111111"},
				{"right@example.com/right-abcdefab2", "d0000000-0000-4000-8000-000000000013_abcdefab-2222-4222-8222-222222222222"},
			} {
				if got := matchKeys(Match(stored, test.selector)); !reflect.DeepEqual(got, []string{test.key}) {
					t.Errorf("long address %q selected %v, want %s", test.selector, got, test.key)
				}
			}
		}},
		{"same organization UUID uses email only to break the hex tie", func(t *testing.T) {
			organizationUUID := "eeeeeeee-eeee-4eee-8eee-eeeeeeeeeeee"
			left := organization("e0000000-0000-4000-8000-000000000014", "left@example.com", organizationUUID, "Acme Corp", "claude_team")
			right := organization("f0000000-0000-4000-8000-000000000015", "right@example.com", organizationUUID, "Renamed", "claude_team")
			stored := []accounts.Account{left, right}
			for _, test := range []struct {
				selector string
				key      string
			}{
				{"left@example.com/acme-corp-eeeeeeee", "e0000000-0000-4000-8000-000000000014_eeeeeeee-eeee-4eee-8eee-eeeeeeeeeeee"},
				{"right@example.com/renamed-eeeeeeee", "f0000000-0000-4000-8000-000000000015_eeeeeeee-eeee-4eee-8eee-eeeeeeeeeeee"},
			} {
				if got := matchKeys(Match(stored, test.selector)); !reflect.DeepEqual(got, []string{test.key}) {
					t.Errorf("full printed address %q selected %v, want %s", test.selector, got, test.key)
				}
			}
			if got := Match(stored, "/acme-corp-eeeeeeee"); len(got) != 2 {
				t.Errorf("short organization form count = %d, want 2", len(got))
			}
		}},
		{"reserved organization and personal namespaces do not collide", func(t *testing.T) {
			team := organization("abcdefab-0000-4000-8000-000000000016", "team@example.com", "12345678-1111-4111-8111-111111111111", "personal", "claude_team")
			personal := organization("12345678-0000-4000-8000-000000000017", "person@example.com", "23456789-2222-4222-8222-222222222222", "Personal", "claude_max")
			stored := []accounts.Account{team, personal}
			for _, test := range []struct {
				selector string
				key      string
			}{
				{"team@example.com/organization-12345678", "abcdefab-0000-4000-8000-000000000016_12345678-1111-4111-8111-111111111111"},
				{"person@example.com/personal-12345678", "12345678-0000-4000-8000-000000000017_23456789-2222-4222-8222-222222222222"},
			} {
				if got := matchKeys(Match(stored, test.selector)); !reflect.DeepEqual(got, []string{test.key}) {
					t.Errorf("reserved namespace match = %v, want %s", got, test.key)
				}
			}
		}},
		{"rename email update and old email reuse never redirect an organization UUID", func(t *testing.T) {
			original := organization("10000000-0000-4000-8000-000000000018", "old@example.com", "01234567-1111-4111-8111-111111111111", "Old Name", "claude_team")
			printed := "old@example.com/old-name-01234567"
			renamed := original
			renamed.OrganizationName = "New Name"
			if got := matchKeys(Match([]accounts.Account{renamed}, printed)); !reflect.DeepEqual(got, []string{"10000000-0000-4000-8000-000000000018_01234567-1111-4111-8111-111111111111"}) {
				t.Errorf("rename match = %v", got)
			}
			updated := renamed
			updated.Email = "new@example.com"
			reuser := organization("20000000-0000-4000-8000-000000000019", "old@example.com", "fedcba98-2222-4222-8222-222222222222", "Other", "claude_team")
			if got := matchKeys(Match([]accounts.Account{updated, reuser}, printed)); !reflect.DeepEqual(got, []string{"10000000-0000-4000-8000-000000000018_01234567-1111-4111-8111-111111111111"}) {
				t.Errorf("email update/reuse match = %v", got)
			}
		}},
		{"removal then replacement retains current-slice behavior", func(t *testing.T) {
			printed := "same@example.com/original-cafebabe"
			replacement := organization("40000000-0000-4000-8000-000000000021", "same@example.com", "cafebabe-2222-4222-8222-222222222222", "Replacement", "claude_team")
			if got := matchKeys(Match([]accounts.Account{replacement}, printed)); !reflect.DeepEqual(got, []string{"40000000-0000-4000-8000-000000000021_cafebabe-2222-4222-8222-222222222222"}) {
				t.Errorf("replacement match = %v, want replacement key", got)
			}
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, tt.run)
	}
}
