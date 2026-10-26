package backend_test

import (
 "testing"

 qt "github.com/frankban/quicktest"
 "github.com/stokaro/ptah-ydb-plugin/internal/backend"
 "github.com/stokaro/ptah-ydb-plugin/internal/streaming"
 "github.com/stokaro/ptah-ydb-plugin/internal/streamingplan"
 "github.com/stokaro/ptah-ydb-plugin/internal/testcomparison"
 "github.com/stokaro/ptah-ydb-plugin/internal/testplanning"
 "github.com/stokaro/ptah-ydb-plugin/schema"
 protocol "github.com/stokaro/ptah/provider-sdk/protocol/experimental"
 "github.com/stokaro/ptah/provider-sdk/provider"
 "google.golang.org/protobuf/proto"
)

func streamingContextState(c *qt.C, representation protocol.ValueInput_Representation, value *protocol.Envelope) *protocol.ComparisonState {
 c.Helper()
 var model *protocol.ValueModel
 for _, candidate:= range schema.Models() {
  if candidate.Kind==schema.StreamingQueryProperties && candidate.Representation==representation { model=candidate }
 }
 c.Assert(model,qt.IsNotNil)
 state:=testcomparison.State(model)
 if value!=nil { appendContextValue(c,state,value) }
 return state
}

func appendContextValue(c *qt.C,state *protocol.ComparisonState,value *protocol.Envelope) {
 c.Helper()
 normalized,err:=backend.New().NormalizeValues(c.Context(),&protocol.NormalizeValuesRequest{Engine:"ydb",TargetFeatures:[]string{provider.PlanningObjectContextFeature,provider.PlanningLifecycleFeature},Values:[]*protocol.ValueInput{{Id:value.Owner.Name,Representation:state.Representation,Value:value}}})
 c.Assert(err,qt.IsNil)
 state.Records=append(state.Records,&protocol.ComparisonRecord{Kind:value.Kind,Subject:testplanning.Subject(value.Owner),Value:normalized.Values[0]})
}

func unchangedStreamingRequest(c *qt.C,run *bool) *protocol.PlanFeaturesRequest {
 c.Helper()
 owner:=&protocol.Reference{Engine:"ydb",Kind:schema.StreamingQueryProperties,Schema:"app",Name:"copy"}
 spec:=streaming.Spec{Text:"SELECT * FROM `app/input`;",Run:run}
 desired,err:=schema.CaptureStreamingQueryIntent(owner,spec,false)
 c.Assert(err,qt.IsNil)
 current,err:=schema.ConvertNative(desired,protocol.ValueInput_DESIRED,protocol.ValueInput_OBSERVED)
 c.Assert(err,qt.IsNil)
 request:=&protocol.PlanFeaturesRequest{Engine:"ydb",PlanningId:streamingplan.ServiceID,DatabasePath:"/local",ContextKinds:[]string{schema.StreamingQueryProperties},TargetFeatures:[]string{provider.PlanningObjectContextFeature,provider.PlanningLifecycleFeature},Capabilities:map[string]bool{streaming.Capability:true,"transactional_ddl":false},Identifiers:&protocol.ComparisonIdentifiers{Rules:proto.CloneOf(backend.Description().Engines[0].Identifiers)},DesiredObjects:streamingContextState(c,protocol.ValueInput_DESIRED,desired),CurrentObjects:streamingContextState(c,protocol.ValueInput_OBSERVED,current)}
 inputOwner:=&protocol.Reference{Engine:"ydb",Kind:schema.TopicProperties,Schema:"app",Name:"input"}
 readOwner:=proto.CloneOf(inputOwner)
 readOwner.Database="/local"
 inputCurrent,err:=schema.CaptureObject(readOwner,readTopic())
 c.Assert(err,qt.IsNil)
 inputDesired,err:=schema.ConvertNative(inputCurrent,protocol.ValueInput_OBSERVED,protocol.ValueInput_DESIRED)
 c.Assert(err,qt.IsNil)
 inputDesired.Owner=proto.CloneOf(inputOwner)
 appendContextValue(c,request.DesiredObjects,inputDesired)
 appendContextValue(c,request.CurrentObjects,inputCurrent)
 request.CurrentObjects.Records[len(request.CurrentObjects.Records)-1].Subject.Database=nil
 request.DesiredObjects.Records[len(request.DesiredObjects.Records)-1].Subject.Database=nil
 request.ChangedSubjects=[]*protocol.ComparisonIdentity{proto.CloneOf(request.CurrentObjects.Records[len(request.CurrentObjects.Records)-1].Subject)}
 return request
}

func TestUnchangedStreamingQueryRestartsAroundChangedInput(t *testing.T) {
 c:=qt.New(t)
 request:=unchangedStreamingRequest(c,nil)
 result,err:=backend.New().PlanFeatures(c.Context(),request)
 c.Assert(err,qt.IsNil)
 c.Assert(result.Diagnostics,qt.HasLen,0)
 c.Assert(result.Changes,qt.HasLen,0)
 c.Assert(result.Objects,qt.HasLen,1)
 c.Assert(result.Objects[0].Steps,qt.HasLen,2)
 steps:=result.Contributions[0].Steps
 c.Assert(steps,qt.HasLen,2)
 c.Assert(steps[0].Metadata.Placement,qt.Equals,protocol.PlanningMetadata_EARLY)
 c.Assert(steps[1].Metadata.Placement,qt.Equals,protocol.PlanningMetadata_DEFAULT)
 rendered:=testplanning.RenderRequest(result,request.Capabilities)
 rendered.Form=protocol.RenderRequest_TEXT
 rendered.TargetFeatures=[]string{provider.TextUnitsFeature}
 reply,err:=backend.New().Render(c.Context(),rendered)
 c.Assert(err,qt.IsNil)
 c.Assert(reply.Units[0].GetSql(),qt.Equals,"ALTER STREAMING QUERY `app/copy` SET (RUN = FALSE, RESOURCE_POOL = `default`);")
 c.Assert(reply.Units[1].GetSql(),qt.Equals,"ALTER STREAMING QUERY `app/copy` SET (RUN = TRUE, RESOURCE_POOL = `default`);")
}

func TestUnchangedStreamingQueryFixedPoints(t *testing.T) {
 for _,test:=range []struct{name string;run *bool;change bool}{{"unchanged input",nil,false},{"stopped query",new(false),true}} {
  t.Run(test.name,func(t *testing.T){
   c:=qt.New(t)
   request:=unchangedStreamingRequest(c,test.run)
   if !test.change { request.ChangedSubjects=nil }
   result,err:=backend.New().PlanFeatures(c.Context(),request)
   c.Assert(err,qt.IsNil)
   c.Assert(result.Diagnostics,qt.HasLen,0)
   c.Assert(result.Contributions,qt.HasLen,0)
   c.Assert(result.Objects,qt.HasLen,1)
   c.Assert(result.Objects[0].Steps,qt.HasLen,0)
  })
 }
}

func TestUnchangedStreamingQueryRefusesMissingInput(t *testing.T) {
 c:=qt.New(t)
 request:=unchangedStreamingRequest(c,nil)
 request.DesiredObjects.Records=request.DesiredObjects.Records[:1]
 result,err:=backend.New().PlanFeatures(c.Context(),request)
 c.Assert(err,qt.IsNil)
 c.Assert(result.Contributions,qt.HasLen,0)
 c.Assert(result.Objects,qt.HasLen,0)
 c.Assert(result.Diagnostics,qt.HasLen,1)
 c.Assert(result.Diagnostics[0].GetContextObject(),qt.Equals,uint32(0))
 c.Assert(result.Diagnostics[0].Message,qt.Contains,"captured surviving")
}

func TestStreamingCaptureRequiresUnchangedObjectAssessment(t *testing.T) {
 c:=qt.New(t)
 request:=unchangedStreamingRequest(c,nil)
 value:=request.DesiredObjects.Records[0].Value.Value
 c.Assert(value.RequiredFeatures,qt.Contains,provider.PlanningObjectContextFeature)
 reply,err:=backend.New().NormalizeValues(c.Context(),&protocol.NormalizeValuesRequest{Engine:"ydb",Values:[]*protocol.ValueInput{{Id:"query",Representation:protocol.ValueInput_DESIRED,Value:value}}})
 c.Assert(err,qt.IsNotNil)
 c.Assert(reply,qt.IsNil)
}

func TestUnchangedStreamingQueryRefusesIncompleteObservation(t *testing.T) {
 c:=qt.New(t)
 request:=unchangedStreamingRequest(c,nil)
 request.CurrentObjects.Records=request.CurrentObjects.Records[1:]
 request.CurrentObjects.Coverage.Kinds[0].Sources[0].Outside=testcomparison.Knowledge(protocol.ModelKnowledge_UNINSPECTED)
 result,err:=backend.New().PlanFeatures(c.Context(),request)
 c.Assert(err,qt.IsNil)
 c.Assert(result.Contributions,qt.HasLen,0)
 c.Assert(result.Diagnostics,qt.HasLen,1)
 c.Assert(result.Diagnostics[0].Message,qt.Contains,"accepted create or drop")
}

func TestUnchangedStreamingQueryDoesNotUseAnotherDatabaseInput(t *testing.T) {
 c:=qt.New(t)
 request:=unchangedStreamingRequest(c,nil)
 foreign:=request.DesiredObjects.Records[1]
 foreign.Subject.Database=testcomparison.Part("/other")
 foreign.Value.Value.Owner.Database="/other"
 foreign.Value.NormalizedOwner.Database="/other"
 result,err:=backend.New().PlanFeatures(c.Context(),request)
 c.Assert(err,qt.IsNil)
 c.Assert(result.Contributions,qt.HasLen,0)
 c.Assert(result.Diagnostics,qt.HasLen,1)
 c.Assert(result.Diagnostics[0].Message,qt.Contains,"captured surviving")
}
