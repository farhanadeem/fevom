const PSCI_VERSION: u32 = 0x8400_0000;
const CPU_SUSPEND: u32 = 0x8400_0001;
const CPU_OFF: u32 = 0x8400_0002;
const CPU_ON: u32 = 0x8400_0003;
const AFFINITY_INFO: u32 = 0x8400_0004;
const MIGRATE_INFO_TYPE: u32 = 0x8400_0006;
const SYSTEM_OFF: u32 = 0x8400_0008;
const SYSTEM_RESET: u32 = 0x8400_0009;
const PSCI_FEATURES: u32 = 0x8400_000a;
const SMC64: u32 = 0x4000_0000;

pub const SUCCESS: i64 = 0;
pub const NOT_SUPPORTED: i64 = -1;
pub const INVALID_PARAMETERS: i64 = -2;
pub const ALREADY_ON: i64 = -4;

const VERSION_1_0: i64 = 0x1_0000;
const TOS_NOT_PRESENT: i64 = 2;
const MPIDR_AFFINITY_MASK: u64 = 0xff_00ff_ffff;

#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub enum Power {
    Off,
    OnPending { entry: u64, context: u64 },
    On,
}

#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub enum Action {
    Return(i64),
    CpuOn { target: usize },
    CpuOff,
    SystemOff,
    SystemReset,
}

pub struct Psci {
    mpidrs: Vec<u64>,
    power: Vec<Power>,
}

impl Psci {
    pub fn new(mpidrs: Vec<u64>) -> Self {
        let mut power = vec![Power::Off; mpidrs.len()];
        power[0] = Power::On;
        Self { mpidrs, power }
    }

    pub fn set_boot(&mut self, entry: u64, context: u64) {
        self.power[0] = Power::OnPending { entry, context };
    }

    pub fn power(&self, cpu: usize) -> Power {
        self.power[cpu]
    }

    pub fn started(&mut self, cpu: usize) -> Option<(u64, u64)> {
        match self.power[cpu] {
            Power::OnPending { entry, context } => {
                self.power[cpu] = Power::On;
                Some((entry, context))
            }
            _ => None,
        }
    }

    fn find(&self, mpidr: u64) -> Option<usize> {
        let want = mpidr & MPIDR_AFFINITY_MASK;
        self.mpidrs.iter().position(|m| *m == want)
    }

    pub fn call(&mut self, caller: usize, x: [u64; 4]) -> Action {
        let function = x[0] as u32;
        match function & !SMC64 {
            PSCI_VERSION => Action::Return(VERSION_1_0),
            CPU_SUSPEND => Action::Return(SUCCESS),
            CPU_OFF => {
                self.power[caller] = Power::Off;
                Action::CpuOff
            }
            CPU_ON => {
                let Some(target) = self.find(x[1]) else {
                    return Action::Return(INVALID_PARAMETERS);
                };
                match self.power[target] {
                    Power::On | Power::OnPending { .. } => Action::Return(ALREADY_ON),
                    Power::Off => {
                        self.power[target] = Power::OnPending {
                            entry: x[2],
                            context: x[3],
                        };
                        Action::CpuOn { target }
                    }
                }
            }
            AFFINITY_INFO => {
                if x[2] != 0 {
                    return Action::Return(INVALID_PARAMETERS);
                }
                match self.find(x[1]).map(|t| self.power[t]) {
                    None => Action::Return(INVALID_PARAMETERS),
                    Some(Power::On | Power::OnPending { .. }) => Action::Return(0),
                    Some(Power::Off) => Action::Return(1),
                }
            }
            MIGRATE_INFO_TYPE => Action::Return(TOS_NOT_PRESENT),
            SYSTEM_OFF => Action::SystemOff,
            SYSTEM_RESET => Action::SystemReset,
            PSCI_FEATURES => {
                let supported = matches!(
                    x[1] as u32 & !SMC64,
                    PSCI_VERSION
                        | CPU_SUSPEND
                        | CPU_OFF
                        | CPU_ON
                        | AFFINITY_INFO
                        | MIGRATE_INFO_TYPE
                        | SYSTEM_OFF
                        | SYSTEM_RESET
                        | PSCI_FEATURES
                );
                Action::Return(if supported { SUCCESS } else { NOT_SUPPORTED })
            }
            _ => Action::Return(NOT_SUPPORTED),
        }
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    fn psci() -> Psci {
        Psci::new(vec![0, 1, 0x100])
    }

    #[test]
    fn reports_version_and_features() {
        let mut p = psci();
        assert_eq!(p.call(0, [0x8400_0000, 0, 0, 0]), Action::Return(0x1_0000));
        assert_eq!(
            p.call(0, [0x8400_000a, 0xc400_0003, 0, 0]),
            Action::Return(SUCCESS)
        );
        assert_eq!(
            p.call(0, [0x8400_000a, 0x8000_0000, 0, 0]),
            Action::Return(NOT_SUPPORTED)
        );
        assert_eq!(
            p.call(0, [0x8000_0001, 0, 0, 0]),
            Action::Return(NOT_SUPPORTED)
        );
    }

    #[test]
    fn cpu_on_moves_through_pending_to_on() {
        let mut p = psci();
        assert_eq!(p.call(0, [0xc400_0004, 1, 0, 0]), Action::Return(1));
        assert_eq!(
            p.call(0, [0xc400_0003, 1, 0x4008_0000, 0xabc]),
            Action::CpuOn { target: 1 }
        );
        assert_eq!(p.call(0, [0xc400_0004, 1, 0, 0]), Action::Return(0));
        assert_eq!(
            p.call(2, [0xc400_0003, 1, 0, 0]),
            Action::Return(ALREADY_ON)
        );
        assert_eq!(p.started(1), Some((0x4008_0000, 0xabc)));
        assert_eq!(p.started(1), None);
        assert_eq!(p.call(0, [0xc400_0004, 1, 0, 0]), Action::Return(0));
        assert_eq!(
            p.call(0, [0xc400_0003, 1, 0, 0]),
            Action::Return(ALREADY_ON)
        );
    }

    #[test]
    fn cpu_on_matches_affinity_and_rejects_unknown_cpus() {
        let mut p = psci();
        assert_eq!(
            p.call(0, [0xc400_0003, 0x8000_0100, 0x1000, 0]),
            Action::CpuOn { target: 2 }
        );
        assert_eq!(
            p.call(0, [0xc400_0003, 5, 0x1000, 0]),
            Action::Return(INVALID_PARAMETERS)
        );
        assert_eq!(
            p.call(0, [0xc400_0003, 0, 0x1000, 0]),
            Action::Return(ALREADY_ON)
        );
    }

    #[test]
    fn cpu_off_then_on_again() {
        let mut p = psci();
        p.call(0, [0xc400_0003, 1, 0x1000, 0]);
        p.started(1);
        assert_eq!(p.call(1, [0x8400_0002, 0, 0, 0]), Action::CpuOff);
        assert_eq!(p.power(1), Power::Off);
        assert_eq!(
            p.call(0, [0xc400_0003, 1, 0x2000, 9]),
            Action::CpuOn { target: 1 }
        );
        assert_eq!(p.started(1), Some((0x2000, 9)));
    }

    #[test]
    fn system_off_and_reset() {
        let mut p = psci();
        assert_eq!(p.call(0, [0x8400_0008, 0, 0, 0]), Action::SystemOff);
        assert_eq!(p.call(0, [0x8400_0009, 0, 0, 0]), Action::SystemReset);
    }
}
