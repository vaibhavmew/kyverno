package validatingadmissionpolicy

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/kyverno/kyverno/pkg/clients/dclient"
	engineapi "github.com/kyverno/kyverno/pkg/engine/api"
	celutils "github.com/kyverno/kyverno/pkg/utils/cel"
	kubeutils "github.com/kyverno/kyverno/pkg/utils/kube"
	"golang.org/x/text/cases"
	"golang.org/x/text/language"
	admissionregistrationv1 "k8s.io/api/admissionregistration/v1"
	"k8s.io/api/admissionregistration/v1alpha1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apiserver/pkg/admission"
	"k8s.io/apiserver/pkg/admission/plugin/cel"
	"k8s.io/apiserver/pkg/admission/plugin/validatingadmissionpolicy"
	"k8s.io/apiserver/pkg/admission/plugin/validatingadmissionpolicy/matching"
	"k8s.io/apiserver/pkg/admission/plugin/webhook/matchconditions"
	celconfig "k8s.io/apiserver/pkg/apis/cel"
)

func GetKinds(policy v1alpha1.ValidatingAdmissionPolicy) []string {
	var kindList []string

	matchResources := policy.Spec.MatchConstraints
	for _, rule := range matchResources.ResourceRules {
		group := rule.APIGroups[0]
		version := rule.APIVersions[0]
		for _, resource := range rule.Resources {
			isSubresource := kubeutils.IsSubresource(resource)
			if isSubresource {
				parts := strings.Split(resource, "/")

				kind := cases.Title(language.English, cases.NoLower).String(parts[0])
				kind, _ = strings.CutSuffix(kind, "s")
				subresource := parts[1]

				if group == "" {
					kindList = append(kindList, strings.Join([]string{version, kind, subresource}, "/"))
				} else {
					kindList = append(kindList, strings.Join([]string{group, version, kind, subresource}, "/"))
				}
			} else {
				resource = cases.Title(language.English, cases.NoLower).String(resource)
				resource, _ = strings.CutSuffix(resource, "s")
				kind := resource

				if group == "" {
					kindList = append(kindList, strings.Join([]string{version, kind}, "/"))
				} else {
					kindList = append(kindList, strings.Join([]string{group, version, kind}, "/"))
				}
			}
		}
	}

	return kindList
}

func Validate(policyData PolicyData, resource unstructured.Unstructured, client dclient.Interface) (engineapi.EngineResponse, error) {
	var (
		gvr schema.GroupVersionResource
		a   admission.Attributes
		err error
	)

	policy := policyData.definition
	bindings := policyData.bindings
	engineResponse := engineapi.NewEngineResponse(resource, engineapi.NewValidatingAdmissionPolicy(policy), nil)
	if client != nil {
		nsLister := NewCustomNamespaceLister(client)
		matcher := validatingadmissionpolicy.NewMatcher(matching.NewMatcher(nsLister, client.GetKubeClient()))

		// convert policy from v1alpha1 to v1beta1
		v1beta1policy := ConvertValidatingAdmissionPolicy(policy)

		// construct admission attributes
		gvr, err = client.Discovery().GetGVRFromGVK(resource.GroupVersionKind())
		if err != nil {
			return engineResponse, err
		}
		a = admission.NewAttributesRecord(resource.DeepCopyObject(), nil, resource.GroupVersionKind(), resource.GetNamespace(), resource.GetName(), gvr, "", admission.Create, nil, false, nil)

		// check if policy matches the incoming resource
		o := admission.NewObjectInterfacesFromScheme(runtime.NewScheme())
		isMatch, _, _, err := matcher.DefinitionMatches(a, o, &v1beta1policy)
		if err != nil {
			return engineResponse, err
		}
		if !isMatch {
			return engineResponse, nil
		}

		if len(bindings) == 0 {
			a = admission.NewAttributesRecord(resource.DeepCopyObject(), nil, resource.GroupVersionKind(), resource.GetNamespace(), resource.GetName(), gvr, "", admission.Create, nil, false, nil)
			resPath := fmt.Sprintf("%s/%s/%s", a.GetNamespace(), a.GetKind().Kind, a.GetName())
			logger.V(3).Info("validate resource %s against policy %s", resPath, policy.GetName())
			return validateResource(policy, nil, resource, a)
		} else {
			for i, binding := range bindings {
				// convert policy binding from v1alpha1 to v1beta1
				v1beta1binding := ConvertValidatingAdmissionPolicyBinding(binding)
				isMatch, err := matcher.BindingMatches(a, o, &v1beta1binding)
				if err != nil {
					return engineResponse, err
				}
				if !isMatch {
					continue
				}

				resPath := fmt.Sprintf("%s/%s/%s", a.GetNamespace(), a.GetKind().Kind, a.GetName())
				logger.V(3).Info("validate resource %s against policy %s with binding %s", resPath, policy.GetName(), binding.GetName())
				return validateResource(policy, &bindings[i], resource, a)
			}
		}
	} else {
		a = admission.NewAttributesRecord(resource.DeepCopyObject(), nil, resource.GroupVersionKind(), resource.GetNamespace(), resource.GetName(), gvr, "", admission.Create, nil, false, nil)
		resPath := fmt.Sprintf("%s/%s/%s", a.GetNamespace(), a.GetKind().Kind, a.GetName())
		logger.V(3).Info("validate resource %s against policy %s", resPath, policy.GetName())
		return validateResource(policy, nil, resource, a)
	}

	return engineResponse, nil
}

func validateResource(policy v1alpha1.ValidatingAdmissionPolicy, binding *v1alpha1.ValidatingAdmissionPolicyBinding, resource unstructured.Unstructured, a admission.Attributes) (engineapi.EngineResponse, error) {
	startTime := time.Now()

	engineResponse := engineapi.NewEngineResponse(resource, engineapi.NewValidatingAdmissionPolicy(policy), nil)
	policyResp := engineapi.NewPolicyResponse()
	var ruleResp *engineapi.RuleResponse

	// compile CEL expressions
	matchConditions := ConvertMatchConditionsV1(policy.Spec.MatchConditions)
	compiler, err := celutils.NewCompiler(policy.Spec.Validations, policy.Spec.AuditAnnotations, matchConditions, policy.Spec.Variables)
	if err != nil {
		return engineResponse, err
	}
	hasParam := policy.Spec.ParamKind != nil
	optionalVars := cel.OptionalVariableDeclarations{HasParams: hasParam, HasAuthorizer: false}
	compiler.CompileVariables(optionalVars)

	var failPolicy admissionregistrationv1.FailurePolicyType
	if policy.Spec.FailurePolicy == nil {
		failPolicy = admissionregistrationv1.Fail
	} else {
		failPolicy = admissionregistrationv1.FailurePolicyType(*policy.Spec.FailurePolicy)
	}

	var matchPolicy v1alpha1.MatchPolicyType
	if policy.Spec.MatchConstraints.MatchPolicy == nil {
		matchPolicy = v1alpha1.Equivalent
	} else {
		matchPolicy = *policy.Spec.MatchConstraints.MatchPolicy
	}

	newMatcher := matchconditions.NewMatcher(compiler.CompileMatchExpressions(optionalVars), &failPolicy, "", string(matchPolicy), "")
	validator := validatingadmissionpolicy.NewValidator(
		compiler.CompileValidateExpressions(optionalVars),
		newMatcher,
		compiler.CompileAuditAnnotationsExpressions(optionalVars),
		compiler.CompileMessageExpressions(optionalVars),
		&failPolicy,
	)
	versionedAttr, _ := admission.NewVersionedAttributes(a, a.GetKind(), nil)
	validateResult := validator.Validate(context.TODO(), a.GetResource(), versionedAttr, nil, nil, celconfig.RuntimeCELCostBudget, nil)

	isPass := true
	for _, policyDecision := range validateResult.Decisions {
		if policyDecision.Evaluation == validatingadmissionpolicy.EvalError {
			isPass = false
			ruleResp = engineapi.RuleError(policy.GetName(), engineapi.Validation, policyDecision.Message, nil)
			break
		} else if policyDecision.Action == validatingadmissionpolicy.ActionDeny {
			isPass = false
			ruleResp = engineapi.RuleFail(policy.GetName(), engineapi.Validation, policyDecision.Message)
			break
		}
	}

	if isPass {
		ruleResp = engineapi.RulePass(policy.GetName(), engineapi.Validation, "")
	}

	if binding != nil {
		ruleResp = ruleResp.WithBinding(binding)
	}
	policyResp.Add(engineapi.NewExecutionStats(startTime, time.Now()), *ruleResp)
	engineResponse = engineResponse.WithPolicyResponse(policyResp)

	return engineResponse, nil
}
