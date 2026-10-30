package daemon

import (
	"embed"

	"fev/proto/contract"
)

//go:embed fixtures/*.json
var fixtureFiles embed.FS

type Fixture = contract.Fixture

func Fixtures() ([]Fixture, error) { return contract.Load(fixtureFiles, "fixtures") }

var Schema = map[string]contract.Types{
	MethodHello:    {HelloParams{}, HelloResult{}},
	MethodStatus:   {Empty{}, Status{}},
	MethodShutdown: {ShutdownParams{}, ShutdownResult{}},

	MethodVMStart:   {StartParams{}, Status{}},
	MethodVMStop:    {StopParams{}, Status{}},
	MethodVMRestart: {StartParams{}, Status{}},

	MethodEventsSubscribe:   {Empty{}, EventsSubscribed{}},
	MethodEventsUnsubscribe: {Subscription{}, Empty{}},
	NotifyStateChanged:      {StateChanged{}, nil},
	NotifyConfigChanged:     {ConfigChanged{}, nil},
	NotifyForwardsChanged:   {ForwardsChanged{}, nil},
	NotifyDockerEvent:       {DockerEvent{}, nil},
	NotifyFilesChanged:      {FilesChanged{}, nil},

	MethodConfigDescribe: {Empty{}, ConfigKeys{}},
	MethodConfigList:     {Empty{}, ConfigValues{}},
	MethodConfigGet:      {ConfigGetParams{}, ConfigValues{}},
	MethodConfigSet:      {ConfigSetParams{}, ConfigChanges{}},
	MethodConfigReset:    {ConfigResetParams{}, ConfigChanges{}},
	MethodConfigAdd:      {ConfigListParams{}, ConfigChanges{}},
	MethodConfigRemove:   {ConfigListParams{}, ConfigChanges{}},
	MethodHTTPSTrust:     {HTTPSTrustParams{}, HTTPSStatus{}},

	MethodLogSubscribe:   {LogSubscribeParams{}, Subscription{}},
	MethodLogUnsubscribe: {Subscription{}, Empty{}},
	NotifyLogEntry:       {LogEntry{}, nil},
	NotifyLogBacklogEnd:  {Subscription{}, nil},

	MethodBootTrace:    {Empty{}, Trace{}},
	MethodDockerStatus: {Empty{}, DockerStatus{}},
	MethodForwardsList: {Empty{}, Forwards{}},
	MethodFilesStatus:  {Empty{}, FilesStatus{}},

	MethodDockerRestart: {Empty{}, DockerStatus{}},
	MethodDataReset:     {Empty{}, DataResetResult{}},

	MethodCaffeinateHold: {CaffeinateParams{}, Caffeinated{}},
	MethodForwardsRetry:  {ForwardsRetryParams{}, Forwards{}},
	NotifyPowerSleep:     {PowerSleep{}, nil},
	NotifyPowerWake:      {PowerWake{}, nil},

	MethodStatsSubscribe:   {StatsSubscribeParams{}, Subscription{}},
	MethodStatsUnsubscribe: {Subscription{}, Empty{}},
	NotifyStatsSample:      {StatsSample{}, nil},

	MethodMachinesList:       {Empty{}, Machines{}},
	MethodMachinesInfo:       {MachineName{}, Machine{}},
	MethodMachinesCreate:     {MachinesCreateParams{}, Operation{}},
	MethodMachinesStart:      {MachineName{}, Machine{}},
	MethodMachinesStop:       {MachinesStopParams{}, Machine{}},
	MethodMachinesRestart:    {MachinesStopParams{}, Machine{}},
	MethodMachinesDelete:     {MachinesStopParams{}, Empty{}},
	MethodMachinesClone:      {MachinesRenameParams{}, Machine{}},
	MethodMachinesRename:     {MachinesRenameParams{}, Machine{}},
	MethodMachinesExport:     {MachinesExportParams{}, Operation{}},
	MethodMachinesImport:     {MachinesImportParams{}, Operation{}},
	MethodMachinesSetDefault: {MachineName{}, Machines{}},
	NotifyMachinesChanged:    {MachinesChanged{}, nil},

	MethodSessionStart:  {SessionStartParams{}, SessionStarted{}},
	MethodSessionResize: {SessionResizeParams{}, Empty{}},
	MethodSessionSignal: {SessionSignalParams{}, Empty{}},
	MethodSessionClose:  {SessionID{}, Empty{}},
	NotifySessionExit:   {SessionExit{}, nil},

	MethodOperationGet:      {OperationID{}, Operation{}},
	MethodOperationCancel:   {OperationID{}, Operation{}},
	NotifyOperationProgress: {OperationProgress{}, nil},

	MethodStorageUsage:   {UsageParams{}, StorageUsage{}},
	MethodStorageSalvage: {SalvageParams{}, Operation{}},

	MethodSSHInfo: {Empty{}, SSHInfo{}},

	MethodMemoryReclaim: {ReclaimParams{}, Reclaimed{}},
	MethodStorageCheck:  {StorageCheckParams{}, Operation{}},
	MethodVolumesClone:  {VolumesCloneParams{}, Volume{}},
	MethodVolumesExport: {VolumesExportParams{}, Operation{}},
	MethodVolumesImport: {VolumesImportParams{}, Operation{}},

	MethodDebugStart:  {DebugStartParams{}, DebugStarted{}},
	MethodDebugStatus: {Empty{}, DebugStatus{}},
	MethodDebugReset:  {DebugResetParams{}, DebugReset{}},

	MethodContextsList:      {Empty{}, RemoteContexts{}},
	MethodContextContainers: {ContextRef{}, RemoteContainers{}},
	MethodContextImages:     {ContextRef{}, RemoteImages{}},

	MethodK8sStatus:  {Empty{}, K8sStatus{}},
	MethodK8sEnable:  {Empty{}, Operation{}},
	MethodK8sDisable: {Empty{}, Operation{}},
	MethodK8sReset:   {K8sResetParams{}, Operation{}},
	MethodK8sWatch:   {K8sWatchParams{}, K8sWatching{}},
	MethodK8sUnwatch: {Subscription{}, Empty{}},
	NotifyK8sChanged: {K8sChanged{}, nil},
	MethodK8sGet:     {K8sRef{}, K8sManifest{}},
	MethodK8sApply:   {K8sApplyParams{}, K8sApplyResult{}},
	MethodK8sUpdate:  {K8sUpdateParams{}, K8sApplyResult{}},
	MethodK8sScale:   {K8sScaleParams{}, K8sObject{}},
	MethodK8sRestart: {K8sRef{}, K8sObject{}},
	MethodK8sDelete:  {K8sRef{}, Empty{}},
	MethodK8sLogs:    {K8sLogsParams{}, Subscription{}},
	NotifyK8sLog:     {K8sLog{}, nil},
	NotifyK8sLogEnd:  {K8sLogEnd{}, nil},
	MethodK8sExec:    {K8sExecParams{}, K8sExecStarted{}},
}

func Conforms(data []byte, zero any) error { return contract.Conforms(data, zero) }
