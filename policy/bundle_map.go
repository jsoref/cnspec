package policy

import (
	"context"
	"sort"
	"strings"

	"github.com/hashicorp/go-version"
	"github.com/pkg/errors"
	"go.mondoo.com/cnquery/explorer"
	"go.mondoo.com/cnquery/llx"
	"go.mondoo.com/cnquery/mrn"
)

// PolicyBundleMap is a PolicyBundle with easier access to policies and queries
type PolicyBundleMap struct {
	OwnerMrn string                        `json:"owner_mrn,omitempty"`
	Policies map[string]*Policy            `json:"policies,omitempty"`
	Queries  map[string]*explorer.Mquery   `json:"queries,omitempty"`
	Props    map[string]*explorer.Property `json:"props,omitempty"`
	Code     map[string]*llx.CodeBundle    `json:"code,omitempty"`
	Library  Library                       `json:"library,omitempty"`
}

// NewPolicyBundleMap creates a new empty initialized map
// dataLake (optional) connects an additional data layer which may provide queries/policies
func NewPolicyBundleMap(ownerMrn string) *PolicyBundleMap {
	return &PolicyBundleMap{
		OwnerMrn: ownerMrn,
		Policies: make(map[string]*Policy),
		Queries:  make(map[string]*explorer.Mquery),
		Props:    make(map[string]*explorer.Property),
		Code:     make(map[string]*llx.CodeBundle),
	}
}

// SelectPolicies selects the policies by name from the list given.
// If a given name does not exist in the map, an error will be thrown.
// The final map will only have the given policies selected. This call does not
// remove queries (at this time).
func (b *PolicyBundleMap) SelectPolicies(names []string) error {
	if len(names) == 0 {
		return nil
	}

	filters := map[string]struct{}{}
	var missing []string

	for i := range names {
		name := names[i]
		if _, ok := b.Policies[name]; !ok {
			missing = append(missing, name)
			continue
		}
		filters[name] = struct{}{}
	}

	if len(missing) != 0 {
		return errors.New("failed to find the following policies: " + strings.Join(missing, ", "))
	}

	for name := range b.Policies {
		if _, ok := filters[name]; !ok {
			delete(b.Policies, name)
		}
	}

	return nil
}

// ToList converts the map to a regular bundle
func (p *PolicyBundleMap) ToList() *Bundle {
	res := Bundle{
		OwnerMrn: p.OwnerMrn,
	}
	var i int
	var ids []string

	// policies
	ids = make([]string, len(p.Policies))
	i = 0
	for k := range p.Policies {
		ids[i] = k
		i++
	}
	sort.Strings(ids)

	res.Policies = make([]*Policy, len(p.Policies))
	for i := range ids {
		res.Policies[i] = p.Policies[ids[i]]
	}

	// queries
	ids = make([]string, len(p.Queries))
	i = 0
	for k := range p.Queries {
		ids[i] = k
		i++
	}
	sort.Strings(ids)

	res.Queries = make([]*explorer.Mquery, len(p.Queries))
	for i := range ids {
		res.Queries[i] = p.Queries[ids[i]]
	}

	// props
	ids = make([]string, len(p.Props))
	i = 0
	for k := range p.Props {
		ids[i] = k
		i++
	}
	sort.Strings(ids)

	res.Props = make([]*explorer.Property, len(p.Props))
	for i := range ids {
		res.Props[i] = p.Props[ids[i]]
	}

	return &res
}

// PoliciesSortedByDependency sorts policies by their dependencies
// note: the MRN field must be set and dependencies in groups must be specified by MRN
func (p *PolicyBundleMap) PoliciesSortedByDependency() ([]*Policy, error) {
	indexer := map[string]struct{}{}
	var res []*Policy

	for i := range p.Policies {
		policy := p.Policies[i]

		if _, ok := indexer[policy.Mrn]; ok {
			continue
		}

		depRes, err := sortPolicies(policy, p, indexer)
		if err != nil {
			return nil, err
		}

		res = append(res, depRes...)
	}

	return res, nil
}

func sortPolicies(p *Policy, bundle *PolicyBundleMap, indexer map[string]struct{}) ([]*Policy, error) {
	var res []*Policy
	indexer[p.Mrn] = struct{}{}

	for i := range p.Groups {
		group := p.Groups[i]
		for i := range group.Policies {
			policy := group.Policies[i]

			// we only do very cursory sanity checking
			if policy.Mrn == "" {
				return nil, errors.New("failed to sort policies: dependency MRN is empty")
			}

			if _, ok := indexer[policy.Mrn]; ok {
				continue
			}

			dep, ok := bundle.Policies[policy.Mrn]
			if !ok {
				// ignore, since we are only looking to sort the policies of the map
				continue
			}

			depRes, err := sortPolicies(dep, bundle, indexer)
			if err != nil {
				return nil, err
			}

			res = append(res, depRes...)
		}
	}

	res = append(res, p)
	return res, nil
}

// ValidatePolicy against the given bundle
func (p *PolicyBundleMap) ValidatePolicy(ctx context.Context, policy *Policy) error {
	if !mrn.IsValid(policy.Mrn) {
		return errors.New("policy MRN is not valid: " + policy.Mrn)
	}

	for i := range policy.Groups {
		if err := p.validateSpec(ctx, policy.Groups[i], policy.Mrn); err != nil {
			return err
		}
	}

	// semver checks are a bit optional
	if policy.Version != "" {
		_, err := version.NewSemver(policy.Version)
		if err != nil {
			return errors.New("policy '" + policy.Mrn + "' version '" + policy.Version + "' is not a valid semver version")
		}
	}

	return nil
}

func (p *PolicyBundleMap) validateSpec(ctx context.Context, group *PolicyGroup, policyMrn string) error {
	if group == nil {
		return errors.New("spec cannot be nil")
	}

	if group.Filters != nil {
		// since asset filters are run beforehand and don't make it into the report
		// we don't store their code bundles separately
		for _, query := range group.Filters.Items {
			_, err := query.RefreshAsFilter(policyMrn)
			if err != nil {
				return err
			}
		}
	}

	for i := range group.Checks {
		check := group.Checks[i]

		exist, err := p.queryExists(ctx, check.Mrn)
		if err != nil {
			return err
		}

		if check.Action == explorer.Mquery_ADD && exist {
			return errors.New("check already exists, but policy is trying to add it: " + check.Mrn)
		}
		if check.Action == explorer.Mquery_MODIFY && !exist {
			return errors.New("check does not exist, but policy is trying to modify it: " + check.Mrn)
		}
	}

	for i := range group.Queries {
		query := group.Queries[i]

		exist, err := p.queryExists(ctx, query.Mrn)
		if err != nil {
			return err
		}

		if query.Action == explorer.Mquery_ADD && exist {
			return errors.New("query already exists, but policy is trying to add it: " + query.Mrn)
		}
		if query.Action == explorer.Mquery_MODIFY && !exist {
			return errors.New("query does not exist, but policy is trying to modify it: " + query.Mrn)
		}
	}

	for i := range group.Policies {
		policy := group.Policies[i]

		exist, err := p.policyExists(ctx, policy.Mrn)
		if err != nil {
			return err
		}

		// policies can only be modified, not fully embedded. so they must exist
		if !exist {
			return errors.New("policy does not exist, but policy is trying to modify it: " + policy.Mrn)
		}
	}

	return nil
}

func (p *PolicyBundleMap) queryExists(ctx context.Context, mrn string) (bool, error) {
	if _, ok := p.Queries[mrn]; ok {
		return true, nil
	}

	if p.Library != nil {
		x, err := p.Library.QueryExists(ctx, mrn)
		if x {
			// we mark it off for caching purposes
			p.Queries[mrn] = nil
		}

		return x, err
	}

	return false, nil
}

func (p *PolicyBundleMap) policyExists(ctx context.Context, mrn string) (bool, error) {
	if _, ok := p.Policies[mrn]; ok {
		return true, nil
	}

	if p.Library != nil {
		x, err := p.Library.PolicyExists(ctx, mrn)
		if x {
			// we mark it off for caching purposes
			p.Policies[mrn] = nil
		}

		return x, err
	}

	return false, nil
}

// QueryMap extracts all the queries from the policy bundle map
func (bundle *PolicyBundleMap) QueryMap() map[string]*explorer.Mquery {
	res := make(map[string]*explorer.Mquery, len(bundle.Queries))
	for _, v := range bundle.Queries {
		res[v.CodeId] = v
	}
	return res
}

func (bundle *PolicyBundleMap) Add(policy *Policy, queries map[string]*explorer.Mquery) *PolicyBundleMap {
	var id string
	if policy.Mrn != "" {
		id = policy.Mrn
	} else {
		id = policy.Uid
	}

	bundle.Policies[id] = policy
	for k, v := range queries {
		bundle.Queries[k] = v
	}
	return bundle
}
