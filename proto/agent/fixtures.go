package agent

import (
	"embed"

	"fev/proto/contract"
)

//go:embed fixtures/*.json
var fixtureFiles embed.FS

type Fixture = contract.Fixture

func Fixtures() ([]Fixture, error) { return contract.Load(fixtureFiles, "fixtures") }

var Schema = map[string]contract.Types{
	MethodHello:           {HelloParams{}, HelloResult{}},
	MethodPing:            {Empty{}, Empty{}},
	MethodInfo:            {Empty{}, Info{}},
	MethodBootTrace:       {BootTraceParams{}, BootTrace{}},
	MethodShutdown:        {ShutdownParams{}, Empty{}},
	MethodLogSubscribe:    {LogSubscribeParams{}, Subscription{}},
	MethodLogUnsubscribe:  {Subscription{}, Empty{}},
	MethodFetch:           {FetchParams{}, FetchResult{}},
	MethodPingHost:        {PingParams{}, PingResult{}},
	MethodViewStart:       {ViewStartParams{}, ViewStatus{}},
	MethodViewStop:        {Empty{}, ViewStatus{}},
	MethodViewStatus:      {Empty{}, ViewStatus{}},
	NotifyViewChanged:     {ViewStatus{}, nil},
	MethodViewObjects:     {Empty{}, ViewObjectList{}},
	NotifyViewRemoved:     {ViewRemoved{}, nil},
	MethodEmulationStatus: {Empty{}, EmulationStatus{}},
	MethodEmulationSet:    {EmulationSetParams{}, EmulationStatus{}},
	MethodDebugStatus:     {Empty{}, DebugStatus{}},
	MethodDebugReset:      {DebugResetParams{}, DebugReset{}},
	MethodK8sStart:        {K8sStartParams{}, Operation{}},
	MethodK8sStop:         {K8sStopParams{}, K8sStatus{}},
	MethodK8sCRI:          {K8sCRIParams{}, K8sStatus{}},
	MethodK8sReset:        {K8sResetParams{}, Operation{}},
	MethodK8sStatus:       {Empty{}, K8sStatus{}},
	MethodK8sConfigure:    {K8sConfig{}, K8sStatus{}},
	MethodK8sImagePull:    {K8sImagePullParams{}, K8sImagePulled{}},
	NotifyK8sState:        {K8sStatus{}, nil},
	MethodDomainsSet:      {DomainsParams{}, DomainsApplied{}},
	MethodMacAddressesSet: {MacAddresses{}, Empty{}},
	NotifyLogEntry:        {LogEntry{}, nil},
	NotifyLogBacklogEnd:   {Subscription{}, nil},

	MethodEngineStatus:      {Empty{}, EngineStatus{}},
	MethodEngineStart:       {Empty{}, EngineStatus{}},
	MethodEngineStop:        {EngineStopParams{}, EngineStatus{}},
	MethodEngineRestart:     {EngineStopParams{}, EngineStatus{}},
	MethodEngineSubscribe:   {Empty{}, Subscription{}},
	MethodEngineUnsubscribe: {Subscription{}, Empty{}},
	NotifyEngineState:       {EngineState{}, nil},

	MethodStorageInfo: {Empty{}, StorageInfo{}},
	MethodStorageTrim: {TrimParams{}, TrimResult{}},

	MethodExecStart:  {ExecStartParams{}, ExecStarted{}},
	MethodExecResize: {ExecResizeParams{}, Empty{}},
	MethodExecSignal: {ExecSignalParams{}, Empty{}},
	MethodExecClose:  {ExecID{}, Empty{}},
	NotifyExecExit:   {ExecExit{}, nil},

	NotifyShutdownProgress: {ShutdownProgress{}, nil},

	MethodFileShareWatch:  {WatchParams{}, WatchResult{}},
	MethodEngineConfigure: {EngineConfigureParams{}, EngineConfigured{}},
	MethodStreamRefuse:    {StreamRefuseParams{}, Empty{}},
	NotifyStreamRequest:   {StreamRequest{}, nil},

	MethodTimeSet: {TimeSetParams{}, TimeSet{}},

	MethodMemoryReclaim: {ReclaimParams{}, Reclaimed{}},

	MethodStatsSubscribe:   {StatsSubscribeParams{}, Subscription{}},
	MethodStatsUnsubscribe: {Subscription{}, Empty{}},
	NotifyStatsSample:      {StatsSample{}, nil},
	NotifyFileShareState:   {FileShareState{}, nil},

	MethodStorageSync:   {Empty{}, Synced{}},
	MethodStorageBudget: {BudgetParams{}, Budget{}},
	MethodFileShareBind: {ShareBindParams{}, ShareBound{}},

	MethodMachineStart:      {MachineStartParams{}, MachineInfo{}},
	MethodMachineStop:       {MachineStopParams{}, MachineInfo{}},
	MethodMachineRestart:    {MachineRestartParams{}, MachineInfo{}},
	MethodMachineInfo:       {MachineNameParams{}, MachineInfo{}},
	MethodMachineList:       {Empty{}, MachineList{}},
	NotifyMachineState:      {MachineState{}, nil},
	MethodStorageCheck:      {CheckParams{}, Operation{}},
	NotifyOperationProgress: {OperationProgress{}, nil},
	MethodStorageSalvage:    {SalvageParams{}, Operation{}},
	MethodStorageUsage:      {UsageParams{}, StorageUsage{}},
	MethodMachineCreate:     {MachineCreateParams{}, Operation{}},
	MethodMachineDelete:     {MachineStopParams{}, Empty{}},
	MethodMachineClone:      {MachineCloneParams{}, MachineInfo{}},
	MethodMachineRename:     {MachineRenameParams{}, MachineInfo{}},
	MethodMachineExport:     {MachineNameParams{}, Operation{}},
	MethodMachineImport:     {MachineImportParams{}, Operation{}},
	MethodOperationCancel:   {OperationID{}, Empty{}},
	MethodMachinePorts:      {Empty{}, MachinePortsList{}},
	NotifyMachinePorts:      {MachinePorts{}, nil},
	MethodEnginePorts:       {Empty{}, EnginePorts{}},
	NotifyEnginePorts:       {EnginePorts{}, nil},
	MethodTrustSet:          {TrustParams{}, Trusted{}},
	MethodSSHAgentSet:       {SSHAgentParams{}, SSHAgentBound{}},
	MethodVolumeClone:       {VolumeCloneParams{}, VolumeInfo{}},
	MethodVolumeExport:      {VolumeNameParams{}, Operation{}},
	MethodVolumeImport:      {VolumeNameParams{}, Operation{}},
}

func Conforms(data []byte, zero any) error { return contract.Conforms(data, zero) }
