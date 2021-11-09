package policyreport

import (
	"encoding/json"
	"fmt"
	"reflect"
	"strings"

	"github.com/cornelk/hashmap"
	changerequest "github.com/kyverno/kyverno/pkg/api/kyverno/v1alpha2"
	report "github.com/kyverno/kyverno/pkg/api/policyreport/v1alpha2"
	kyvernoclient "github.com/kyverno/kyverno/pkg/client/clientset/versioned"
	changerequestlister "github.com/kyverno/kyverno/pkg/client/listers/kyverno/v1alpha2"
	policyreportlister "github.com/kyverno/kyverno/pkg/client/listers/policyreport/v1alpha2"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

type PolicyReportEraser interface {
	CleanupReportChangeRequests(cleanup CleanupReportChangeRequests) error
	EraseResultsEntries(erase EraseResultsEntries) error
}

type CleanupReportChangeRequests = func(pclient *kyvernoclient.Clientset, rcrLister changerequestlister.ReportChangeRequestLister, crcrLister changerequestlister.ClusterReportChangeRequestLister) error
type EraseResultsEntries = func(pclient *kyvernoclient.Clientset, reportLister policyreportlister.PolicyReportLister, clusterReportLister policyreportlister.ClusterPolicyReportLister) error

func (g *ReportGenerator) CleanupReportChangeRequests(cleanup CleanupReportChangeRequests) error {
	return cleanup(g.pclient, g.reportChangeRequestLister, g.clusterReportChangeRequestLister)
}

func (g *ReportGenerator) EraseResultsEntries(erase EraseResultsEntries) error {
	return erase(g.pclient, g.reportLister, g.clusterReportLister)
}

type deletedResource struct {
	kind, ns, name string
}

func buildLabelForDeletedResource(labels, annotations map[string]string) *deletedResource {
	ok := true
	kind, kindOk := annotations[deletedAnnotationResourceKind]
	ok = ok && kindOk

	name, nameOk := annotations[deletedAnnotationResourceName]
	ok = ok && nameOk

	if !ok {
		return nil
	}

	return &deletedResource{
		kind: kind,
		name: name,
		ns:   labels[resourceLabelNamespace],
	}
}

func getDeletedResources(aggregatedRequests interface{}) (resources []deletedResource) {
	if requests, ok := aggregatedRequests.([]*changerequest.ClusterReportChangeRequest); ok {
		for _, request := range requests {
			dr := buildLabelForDeletedResource(request.GetLabels(), request.GetAnnotations())
			if dr != nil {
				resources = append(resources, *dr)
			}
		}
	} else if requests, ok := aggregatedRequests.([]*changerequest.ReportChangeRequest); ok {
		for _, request := range requests {
			dr := buildLabelForDeletedResource(request.GetLabels(), request.GetAnnotations())
			if dr != nil {
				resources = append(resources, *dr)
			}
		}
	}
	return
}

func updateResults(oldReport, newReport map[string]interface{}, aggregatedRequests interface{}) (map[string]interface{}, bool, error) {
	deleteResources := getDeletedResources(aggregatedRequests)
	oldResults := hashResults(oldReport, deleteResources)
	var hasDuplicate bool

	if newresults, ok := newReport["results"].([]interface{}); ok {
		for _, res := range newresults {
			resMap, ok := res.(map[string]interface{})
			if !ok {
				continue
			}
			if key, ok := generateHashKey(resMap, deletedResource{}); ok {
				if _, exist := oldResults.Get(key); exist {
					hasDuplicate = exist
				}

				oldResults.Set(key, res)
			}
		}
	}

	results := getResultsFromHash(oldResults)
	if err := unstructured.SetNestedSlice(newReport, results, "results"); err != nil {
		return nil, hasDuplicate, err
	}

	summaryResults := []report.PolicyReportResult{}
	if err := mapToStruct(results, &summaryResults); err != nil {
		return nil, hasDuplicate, err
	}

	summary := updateSummary(summaryResults)
	if err := unstructured.SetNestedMap(newReport, summary.ToMap(), "summary"); err != nil {
		return nil, hasDuplicate, err
	}
	return newReport, hasDuplicate, nil
}

func hashResults(policyReport map[string]interface{}, deleteResources []deletedResource) *hashmap.HashMap {
	resultsHash := &hashmap.HashMap{}

	results, ok := policyReport["results"]
	if !ok {
		return resultsHash
	}

	for _, result := range results.([]interface{}) {
		if len(deleteResources) != 0 {
			for _, dr := range deleteResources {
				if key, ok := generateHashKey(result.(map[string]interface{}), dr); ok {
					resultsHash.Set(key, result)
				}
			}
		} else {
			if key, ok := generateHashKey(result.(map[string]interface{}), deletedResource{}); ok {
				resultsHash.Set(key, result)
			}
		}
	}
	return resultsHash
}

func getResultsFromHash(resHash *hashmap.HashMap) []interface{} {
	results := make([]interface{}, 0)

	for result := range resHash.Iter() {
		if reflect.DeepEqual(result, hashmap.KeyValue{}) {
			continue
		}

		results = append(results, result.Value.(map[string]interface{}))

	}
	return results
}

func generateHashKey(result map[string]interface{}, dr deletedResource) (string, bool) {
	resources := result["resources"].([]interface{})
	if len(resources) < 1 {
		return "", false
	}

	resource := resources[0].(map[string]interface{})
	if !reflect.DeepEqual(dr, deletedResource{}) {
		if resource["kind"] == dr.kind {
			if resource["name"] == dr.name && resource["namespace"] == dr.ns {
				return "", false
			}

			if dr.kind == "Namespace" && resource["name"] == dr.name {
				return "", false
			}
		}

	}

	return fmt.Sprintf(
		"%s-%s-%s-%s-%s",
		result["policy"],
		result["rule"],
		resource["kind"],
		resource["namespace"],
		resource["name"]), true
}

func updateSummary(results []report.PolicyReportResult) report.PolicyReportSummary {
	summary := report.PolicyReportSummary{}

	for _, result := range results {
		switch result.Result {
		case report.StatusPass:
			summary.Pass++
		case report.StatusFail:
			summary.Fail++
		case report.StatusWarn:
			summary.Warn++
		case report.StatusError:
			summary.Error++
		case report.StatusSkip:
			summary.Skip++
		}
	}

	return summary
}

func isDeletedPolicyKey(key string) (policyName, ruleName string, isDelete bool) {
	policy := strings.Split(key, "/")

	if policy[0] == deletedPolicyKey {
		// deletedPolicyKey/policyName/ruleName
		if len(policy) == 3 {
			return policy[1], policy[2], true
		}
		// deletedPolicyKey/policyName
		if len(policy) == 2 {
			return policy[1], "", true
		}
	}

	return "", "", false
}

func mapToStruct(in, out interface{}) error {
	jsonBytes, _ := json.Marshal(in)
	return json.Unmarshal(jsonBytes, out)
}
