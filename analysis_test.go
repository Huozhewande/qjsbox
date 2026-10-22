package qjsbox

import (
	"context"
	"testing"
)

func TestAnalysisCapturesGeneratedHelperOperands(t *testing.T) {
	r := testRunner(t)
	a, err := r.Analyze(context.Background(), "RULE", `return __rule.any([()=>__rule.test('user.region','IN',['eu','test-region']),()=>__rule.test('model.score','GTE',-2),()=>__rule.test('text.present','EXISTS')]);`)
	if err != nil {
		t.Fatal(err)
	}
	if len(a.HelperTests) != 3 || a.HelperTests[0].Reference != "user.region" || a.HelperTests[0].Operator != "IN" || string(a.HelperTests[0].Operand) != `["eu","test-region"]` || string(a.HelperTests[1].Operand) != "-2" || len(a.HelperTests[2].Operand) != 0 {
		t.Fatalf("literal choice metadata lost: %+v", a)
	}
	plain, err := r.Analyze(context.Background(), "RULE", `// __rule.test('user.region','EQUALS','not-code')
return factors.good;`)
	if err != nil || len(plain.HelperTests) != 0 || len(plain.References) != 1 || plain.References[0] != "good" {
		t.Fatalf("comment treated as helper: %+v %v", plain, err)
	}
}
