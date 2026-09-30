package cpp

import "strings"

// cppHeaderMacros are the macros -- object-like and function-like -- that the
// headers a generated C++ file reaches define under a spelling a schema name,
// or a type identifier built from schema names, can take: the header's own
// includes (<cstdint> <string> <vector> <span> <cstddef> <utility> <variant>
// <new> <memory>), the corelib's sofab/sofab.hpp (and corelib-c-cpp's
// sofab/object.h), and the project harness's (<iostream> <sstream> <fstream>
// <ostream> <cstdio> <cstdlib> <cstring>, sofab_test_json.h), in -std=c++20
// and -std=gnu++20 (which adds `linux` and `unix`).
//
// The preprocessor replaces an identifier that is one of these before the
// compiler sees it: a member `NULL` becomes a null pointer constant, a type
// `INT8_MAX` a number, an enumerator `stdin` an expression -- so a name here
// takes a trailing underscore wherever this backend spells an identifier
// (reserved.go). The SOFAB_* macros are not listed: every identifier starting
// with "SOFAB" and an underscore is escaped by prefix (sofabPrefixed), which
// also covers the corelib's future macros and the ones this backend emits.
//
// Measured rather than written by hand: glibc and libstdc++ define several
// hundred such names beyond the ISO ones (errno constants, SYS_*, PTHREAD_*),
// and a hand-written list misses them. Regenerated with g++ over a TU that
// includes the headers above, against each corelib, in both language modes:
//
//	g++ -std=c++20 -dM -E -I<corelib include dirs> probe.cpp |
//	    awk '{print $2}' | sed 's/(.*//' |
//	    grep -E '^[A-Za-z][A-Za-z0-9]*(_[A-Za-z0-9]+)*$' | grep -v '^SOFAB_'
//
// This one was taken with g++ 15.2 on glibc (x86_64). Another C library
// (musl, newlib) defines the same ISO names plus a few of its own.
var cppHeaderMacros = map[string]bool{}

func init() {
	for _, n := range strings.Fields(cppHeaderMacroList) {
		cppHeaderMacros[n] = true
	}
}

const cppHeaderMacroList = `
ADJ_ESTERROR ADJ_FREQUENCY ADJ_MAXERROR ADJ_MICRO ADJ_NANO ADJ_OFFSET ADJ_OFFSET_SINGLESHOT ADJ_OFFSET_SS_READ
ADJ_SETOFFSET ADJ_STATUS ADJ_TAI ADJ_TICK ADJ_TIMECONST AIO_PRIO_DELTA_MAX ATOMIC_BOOL_LOCK_FREE ATOMIC_CHAR16_T_LOCK_FREE
ATOMIC_CHAR32_T_LOCK_FREE ATOMIC_CHAR8_T_LOCK_FREE ATOMIC_CHAR_LOCK_FREE ATOMIC_FLAG_INIT ATOMIC_INT_LOCK_FREE ATOMIC_LLONG_LOCK_FREE ATOMIC_LONG_LOCK_FREE ATOMIC_POINTER_LOCK_FREE
ATOMIC_SHORT_LOCK_FREE ATOMIC_VAR_INIT ATOMIC_WCHAR_T_LOCK_FREE AT_RENAME_EXCHANGE AT_RENAME_NOREPLACE AT_RENAME_WHITEOUT BC_BASE_MAX BC_DIM_MAX
BC_SCALE_MAX BC_STRING_MAX BIG_ENDIAN BOOL_MAX BOOL_WIDTH BUFSIZ BYTE_ORDER CHARCLASS_NAME_MAX
CHAR_BIT CHAR_MAX CHAR_MIN CHAR_WIDTH CLOCKS_PER_SEC CLOCK_BOOTTIME CLOCK_BOOTTIME_ALARM CLOCK_MONOTONIC
CLOCK_MONOTONIC_COARSE CLOCK_MONOTONIC_RAW CLOCK_PROCESS_CPUTIME_ID CLOCK_REALTIME CLOCK_REALTIME_ALARM CLOCK_REALTIME_COARSE CLOCK_TAI CLOCK_THREAD_CPUTIME_ID
CLONE_CHILD_CLEARTID CLONE_CHILD_SETTID CLONE_DETACHED CLONE_FILES CLONE_FS CLONE_IO CLONE_NEWCGROUP CLONE_NEWIPC
CLONE_NEWNET CLONE_NEWNS CLONE_NEWPID CLONE_NEWTIME CLONE_NEWUSER CLONE_NEWUTS CLONE_PARENT CLONE_PARENT_SETTID
CLONE_PIDFD CLONE_PTRACE CLONE_SETTLS CLONE_SIGHAND CLONE_SYSVSEM CLONE_THREAD CLONE_UNTRACED CLONE_VFORK
CLONE_VM CLOSE_RANGE_CLOEXEC CLOSE_RANGE_UNSHARE COLL_WEIGHTS_MAX CPU_ALLOC CPU_ALLOC_SIZE CPU_AND CPU_AND_S
CPU_CLR CPU_CLR_S CPU_COUNT CPU_COUNT_S CPU_EQUAL CPU_EQUAL_S CPU_FREE CPU_ISSET
CPU_ISSET_S CPU_OR CPU_OR_S CPU_SET CPU_SETSIZE CPU_SET_S CPU_XOR CPU_XOR_S
CPU_ZERO CPU_ZERO_S CSIGNAL DELAYTIMER_MAX E2BIG EACCES EADDRINUSE EADDRNOTAVAIL
EADV EAFNOSUPPORT EAGAIN EALREADY EBADE EBADF EBADFD EBADMSG
EBADR EBADRQC EBADSLT EBFONT EBUSY ECANCELED ECHILD ECHRNG
ECOMM ECONNABORTED ECONNREFUSED ECONNRESET EDEADLK EDEADLOCK EDESTADDRREQ EDOM
EDOTDOT EDQUOT EEXIST EFAULT EFBIG EFSBADCRC EFSCORRUPTED EHOSTDOWN
EHOSTUNREACH EHWPOISON EIDRM EILSEQ EINPROGRESS EINTR EINVAL EIO
EISCONN EISDIR EISNAM EKEYEXPIRED EKEYREJECTED EKEYREVOKED EL2HLT EL2NSYNC
EL3HLT EL3RST ELIBACC ELIBBAD ELIBEXEC ELIBMAX ELIBSCN ELNRNG
ELOOP EMEDIUMTYPE EMFILE EMLINK EMSGSIZE EMULTIHOP ENAMETOOLONG ENAVAIL
ENETDOWN ENETRESET ENETUNREACH ENFILE ENOANO ENOBUFS ENOCSI ENODATA
ENODEV ENOENT ENOEXEC ENOKEY ENOLCK ENOLINK ENOMEDIUM ENOMEM
ENOMSG ENONET ENOPKG ENOPROTOOPT ENOSPC ENOSR ENOSTR ENOSYS
ENOTBLK ENOTCONN ENOTDIR ENOTEMPTY ENOTNAM ENOTRECOVERABLE ENOTSOCK ENOTSUP
ENOTTY ENOTUNIQ ENXIO EOF EOPNOTSUPP EOVERFLOW EOWNERDEAD EPERM
EPFNOSUPPORT EPIPE EPROTO EPROTONOSUPPORT EPROTOTYPE ERANGE EREMCHG EREMOTE
EREMOTEIO ERESTART ERFKILL EROFS ESHUTDOWN ESOCKTNOSUPPORT ESPIPE ESRCH
ESRMNT ESTALE ESTRPIPE ETIME ETIMEDOUT ETOOMANYREFS ETXTBSY EUCLEAN
EUNATCH EUSERS EWOULDBLOCK EXDEV EXFULL EXIT_FAILURE EXIT_SUCCESS EXPR_NEST_MAX
FD_CLR FD_ISSET FD_SET FD_SETSIZE FD_ZERO FILENAME_MAX FOPEN_MAX F_LOCK
F_OK F_TEST F_TLOCK F_ULOCK HOST_NAME_MAX INT16_C INT16_MAX INT16_MIN
INT16_WIDTH INT32_C INT32_MAX INT32_MIN INT32_WIDTH INT64_C INT64_MAX INT64_MIN
INT64_WIDTH INT8_C INT8_MAX INT8_MIN INT8_WIDTH INTMAX_C INTMAX_MAX INTMAX_MIN
INTMAX_WIDTH INTPTR_MAX INTPTR_MIN INTPTR_WIDTH INT_FAST16_MAX INT_FAST16_MIN INT_FAST16_WIDTH INT_FAST32_MAX
INT_FAST32_MIN INT_FAST32_WIDTH INT_FAST64_MAX INT_FAST64_MIN INT_FAST64_WIDTH INT_FAST8_MAX INT_FAST8_MIN INT_FAST8_WIDTH
INT_LEAST16_MAX INT_LEAST16_MIN INT_LEAST16_WIDTH INT_LEAST32_MAX INT_LEAST32_MIN INT_LEAST32_WIDTH INT_LEAST64_MAX INT_LEAST64_MIN
INT_LEAST64_WIDTH INT_LEAST8_MAX INT_LEAST8_MIN INT_LEAST8_WIDTH INT_MAX INT_MIN INT_WIDTH IOV_MAX
LC_ADDRESS LC_ADDRESS_MASK LC_ALL LC_ALL_MASK LC_COLLATE LC_COLLATE_MASK LC_CTYPE LC_CTYPE_MASK
LC_GLOBAL_LOCALE LC_IDENTIFICATION LC_IDENTIFICATION_MASK LC_MEASUREMENT LC_MEASUREMENT_MASK LC_MESSAGES LC_MESSAGES_MASK LC_MONETARY
LC_MONETARY_MASK LC_NAME LC_NAME_MASK LC_NUMERIC LC_NUMERIC_MASK LC_PAPER LC_PAPER_MASK LC_TELEPHONE
LC_TELEPHONE_MASK LC_TIME LC_TIME_MASK LINE_MAX LITTLE_ENDIAN LLONG_MAX LLONG_MIN LLONG_WIDTH
LOGIN_NAME_MAX LONG_BIT LONG_LONG_MAX LONG_LONG_MIN LONG_MAX LONG_MIN LONG_WIDTH L_INCR
L_SET L_XTND L_ctermid L_cuserid L_tmpnam MAX_CANON MAX_INPUT MB_CUR_MAX
MB_LEN_MAX MOD_CLKA MOD_CLKB MOD_ESTERROR MOD_FREQUENCY MOD_MAXERROR MOD_MICRO MOD_NANO
MOD_OFFSET MOD_STATUS MOD_TAI MOD_TIMECONST MQ_PRIO_MAX NAME_MAX NFDBITS NGROUPS_MAX
NL_ARGMAX NL_LANGMAX NL_MSGMAX NL_NMAX NL_SETMAX NL_TEXTMAX NULL NZERO
PATH_MAX PDP_ENDIAN PIPE_BUF PTHREAD_ADAPTIVE_MUTEX_INITIALIZER_NP PTHREAD_ATTR_NO_SIGMASK_NP PTHREAD_BARRIER_SERIAL_THREAD PTHREAD_CANCELED PTHREAD_CANCEL_ASYNCHRONOUS
PTHREAD_CANCEL_DEFERRED PTHREAD_CANCEL_DISABLE PTHREAD_CANCEL_ENABLE PTHREAD_COND_INITIALIZER PTHREAD_CREATE_DETACHED PTHREAD_CREATE_JOINABLE PTHREAD_DESTRUCTOR_ITERATIONS PTHREAD_ERRORCHECK_MUTEX_INITIALIZER_NP
PTHREAD_EXPLICIT_SCHED PTHREAD_INHERIT_SCHED PTHREAD_KEYS_MAX PTHREAD_MUTEX_INITIALIZER PTHREAD_ONCE_INIT PTHREAD_PROCESS_PRIVATE PTHREAD_PROCESS_SHARED PTHREAD_RECURSIVE_MUTEX_INITIALIZER_NP
PTHREAD_RWLOCK_INITIALIZER PTHREAD_RWLOCK_WRITER_NONRECURSIVE_INITIALIZER_NP PTHREAD_SCOPE_PROCESS PTHREAD_SCOPE_SYSTEM PTHREAD_STACK_MIN PTRDIFF_MAX PTRDIFF_MIN PTRDIFF_WIDTH
P_tmpdir RAND_MAX RENAME_EXCHANGE RENAME_NOREPLACE RENAME_WHITEOUT RE_DUP_MAX RTSIG_MAX R_OK
SCHAR_MAX SCHAR_MIN SCHAR_WIDTH SCHED_ATTR_SIZE_VER0 SCHED_ATTR_SIZE_VER1 SCHED_BATCH SCHED_DEADLINE SCHED_EXT
SCHED_FIFO SCHED_FLAG_DL_OVERRUN SCHED_FLAG_KEEP_ALL SCHED_FLAG_KEEP_PARAMS SCHED_FLAG_KEEP_POLICY SCHED_FLAG_RECLAIM SCHED_FLAG_RESET_ON_FORK SCHED_FLAG_UTIL_CLAMP
SCHED_FLAG_UTIL_CLAMP_MAX SCHED_FLAG_UTIL_CLAMP_MIN SCHED_IDLE SCHED_ISO SCHED_NORMAL SCHED_OTHER SCHED_RESET_ON_FORK SCHED_RR
SEEK_CUR SEEK_DATA SEEK_END SEEK_HOLE SEEK_SET SEM_VALUE_MAX SHRT_MAX SHRT_MIN
SHRT_WIDTH SIG_ATOMIC_MAX SIG_ATOMIC_MIN SIG_ATOMIC_WIDTH SIZE_MAX SIZE_WIDTH SSIZE_MAX STA_CLK
STA_CLOCKERR STA_DEL STA_FLL STA_FREQHOLD STA_INS STA_MODE STA_NANO STA_PLL
STA_PPSERROR STA_PPSFREQ STA_PPSJITTER STA_PPSSIGNAL STA_PPSTIME STA_PPSWANDER STA_RONLY STA_UNSYNC
STDERR_FILENO STDIN_FILENO STDOUT_FILENO SYS_accept SYS_accept4 SYS_access SYS_acct SYS_add_key
SYS_adjtimex SYS_afs_syscall SYS_alarm SYS_arch_prctl SYS_bind SYS_bpf SYS_brk SYS_cachestat
SYS_capget SYS_capset SYS_chdir SYS_chmod SYS_chown SYS_chroot SYS_clock_adjtime SYS_clock_getres
SYS_clock_gettime SYS_clock_nanosleep SYS_clock_settime SYS_clone SYS_clone3 SYS_close SYS_close_range SYS_connect
SYS_copy_file_range SYS_creat SYS_create_module SYS_delete_module SYS_dup SYS_dup2 SYS_dup3 SYS_epoll_create
SYS_epoll_create1 SYS_epoll_ctl SYS_epoll_ctl_old SYS_epoll_pwait SYS_epoll_pwait2 SYS_epoll_wait SYS_epoll_wait_old SYS_eventfd
SYS_eventfd2 SYS_execve SYS_execveat SYS_exit SYS_exit_group SYS_faccessat SYS_faccessat2 SYS_fadvise64
SYS_fallocate SYS_fanotify_init SYS_fanotify_mark SYS_fchdir SYS_fchmod SYS_fchmodat SYS_fchmodat2 SYS_fchown
SYS_fchownat SYS_fcntl SYS_fdatasync SYS_fgetxattr SYS_file_getattr SYS_file_setattr SYS_finit_module SYS_flistxattr
SYS_flock SYS_fork SYS_fremovexattr SYS_fsconfig SYS_fsetxattr SYS_fsmount SYS_fsopen SYS_fspick
SYS_fstat SYS_fstatfs SYS_fsync SYS_ftruncate SYS_futex SYS_futex_requeue SYS_futex_wait SYS_futex_waitv
SYS_futex_wake SYS_futimesat SYS_get_kernel_syms SYS_get_mempolicy SYS_get_robust_list SYS_get_thread_area SYS_getcpu SYS_getcwd
SYS_getdents SYS_getdents64 SYS_getegid SYS_geteuid SYS_getgid SYS_getgroups SYS_getitimer SYS_getpeername
SYS_getpgid SYS_getpgrp SYS_getpid SYS_getpmsg SYS_getppid SYS_getpriority SYS_getrandom SYS_getresgid
SYS_getresuid SYS_getrlimit SYS_getrusage SYS_getsid SYS_getsockname SYS_getsockopt SYS_gettid SYS_gettimeofday
SYS_getuid SYS_getxattr SYS_getxattrat SYS_init_module SYS_inotify_add_watch SYS_inotify_init SYS_inotify_init1 SYS_inotify_rm_watch
SYS_io_cancel SYS_io_destroy SYS_io_getevents SYS_io_pgetevents SYS_io_setup SYS_io_submit SYS_io_uring_enter SYS_io_uring_register
SYS_io_uring_setup SYS_ioctl SYS_ioperm SYS_iopl SYS_ioprio_get SYS_ioprio_set SYS_kcmp SYS_kexec_file_load
SYS_kexec_load SYS_keyctl SYS_kill SYS_landlock_add_rule SYS_landlock_create_ruleset SYS_landlock_restrict_self SYS_lchown SYS_lgetxattr
SYS_link SYS_linkat SYS_listen SYS_listmount SYS_listxattr SYS_listxattrat SYS_llistxattr SYS_lookup_dcookie
SYS_lremovexattr SYS_lseek SYS_lsetxattr SYS_lsm_get_self_attr SYS_lsm_list_modules SYS_lsm_set_self_attr SYS_lstat SYS_madvise
SYS_map_shadow_stack SYS_mbind SYS_membarrier SYS_memfd_create SYS_memfd_secret SYS_migrate_pages SYS_mincore SYS_mkdir
SYS_mkdirat SYS_mknod SYS_mknodat SYS_mlock SYS_mlock2 SYS_mlockall SYS_mmap SYS_modify_ldt
SYS_mount SYS_mount_setattr SYS_move_mount SYS_move_pages SYS_mprotect SYS_mq_getsetattr SYS_mq_notify SYS_mq_open
SYS_mq_timedreceive SYS_mq_timedsend SYS_mq_unlink SYS_mremap SYS_mseal SYS_msgctl SYS_msgget SYS_msgrcv
SYS_msgsnd SYS_msync SYS_munlock SYS_munlockall SYS_munmap SYS_name_to_handle_at SYS_nanosleep SYS_newfstatat
SYS_nfsservctl SYS_open SYS_open_by_handle_at SYS_open_tree SYS_open_tree_attr SYS_openat SYS_openat2 SYS_pause
SYS_perf_event_open SYS_personality SYS_pidfd_getfd SYS_pidfd_open SYS_pidfd_send_signal SYS_pipe SYS_pipe2 SYS_pivot_root
SYS_pkey_alloc SYS_pkey_free SYS_pkey_mprotect SYS_poll SYS_ppoll SYS_prctl SYS_pread64 SYS_preadv
SYS_preadv2 SYS_prlimit64 SYS_process_madvise SYS_process_mrelease SYS_process_vm_readv SYS_process_vm_writev SYS_pselect6 SYS_ptrace
SYS_putpmsg SYS_pwrite64 SYS_pwritev SYS_pwritev2 SYS_query_module SYS_quotactl SYS_quotactl_fd SYS_read
SYS_readahead SYS_readlink SYS_readlinkat SYS_readv SYS_reboot SYS_recvfrom SYS_recvmmsg SYS_recvmsg
SYS_remap_file_pages SYS_removexattr SYS_removexattrat SYS_rename SYS_renameat SYS_renameat2 SYS_request_key SYS_restart_syscall
SYS_rmdir SYS_rseq SYS_rt_sigaction SYS_rt_sigpending SYS_rt_sigprocmask SYS_rt_sigqueueinfo SYS_rt_sigreturn SYS_rt_sigsuspend
SYS_rt_sigtimedwait SYS_rt_tgsigqueueinfo SYS_sched_get_priority_max SYS_sched_get_priority_min SYS_sched_getaffinity SYS_sched_getattr SYS_sched_getparam SYS_sched_getscheduler
SYS_sched_rr_get_interval SYS_sched_setaffinity SYS_sched_setattr SYS_sched_setparam SYS_sched_setscheduler SYS_sched_yield SYS_seccomp SYS_security
SYS_select SYS_semctl SYS_semget SYS_semop SYS_semtimedop SYS_sendfile SYS_sendmmsg SYS_sendmsg
SYS_sendto SYS_set_mempolicy SYS_set_mempolicy_home_node SYS_set_robust_list SYS_set_thread_area SYS_set_tid_address SYS_setdomainname SYS_setfsgid
SYS_setfsuid SYS_setgid SYS_setgroups SYS_sethostname SYS_setitimer SYS_setns SYS_setpgid SYS_setpriority
SYS_setregid SYS_setresgid SYS_setresuid SYS_setreuid SYS_setrlimit SYS_setsid SYS_setsockopt SYS_settimeofday
SYS_setuid SYS_setxattr SYS_setxattrat SYS_shmat SYS_shmctl SYS_shmdt SYS_shmget SYS_shutdown
SYS_sigaltstack SYS_signalfd SYS_signalfd4 SYS_socket SYS_socketpair SYS_splice SYS_stat SYS_statfs
SYS_statmount SYS_statx SYS_swapoff SYS_swapon SYS_symlink SYS_symlinkat SYS_sync SYS_sync_file_range
SYS_syncfs SYS_sysfs SYS_sysinfo SYS_syslog SYS_tee SYS_tgkill SYS_time SYS_timer_create
SYS_timer_delete SYS_timer_getoverrun SYS_timer_gettime SYS_timer_settime SYS_timerfd_create SYS_timerfd_gettime SYS_timerfd_settime SYS_times
SYS_tkill SYS_truncate SYS_tuxcall SYS_umask SYS_umount2 SYS_uname SYS_unlink SYS_unlinkat
SYS_unshare SYS_uretprobe SYS_uselib SYS_userfaultfd SYS_ustat SYS_utime SYS_utimensat SYS_utimes
SYS_vfork SYS_vhangup SYS_vmsplice SYS_vserver SYS_wait4 SYS_waitid SYS_write SYS_writev
TEMP_FAILURE_RETRY TIMER_ABSTIME TIME_ACTIVE TIME_MONOTONIC TIME_THREAD_ACTIVE TIME_UTC TMP_MAX TTY_NAME_MAX
UCHAR_MAX UCHAR_WIDTH UINT16_C UINT16_MAX UINT16_WIDTH UINT32_C UINT32_MAX UINT32_WIDTH
UINT64_C UINT64_MAX UINT64_WIDTH UINT8_C UINT8_MAX UINT8_WIDTH UINTMAX_C UINTMAX_MAX
UINTMAX_WIDTH UINTPTR_MAX UINTPTR_WIDTH UINT_FAST16_MAX UINT_FAST16_WIDTH UINT_FAST32_MAX UINT_FAST32_WIDTH UINT_FAST64_MAX
UINT_FAST64_WIDTH UINT_FAST8_MAX UINT_FAST8_WIDTH UINT_LEAST16_MAX UINT_LEAST16_WIDTH UINT_LEAST32_MAX UINT_LEAST32_WIDTH UINT_LEAST64_MAX
UINT_LEAST64_WIDTH UINT_LEAST8_MAX UINT_LEAST8_WIDTH UINT_MAX UINT_WIDTH ULLONG_MAX ULLONG_WIDTH ULONG_LONG_MAX
ULONG_MAX ULONG_WIDTH USHRT_MAX USHRT_WIDTH WCHAR_MAX WCHAR_MIN WCHAR_WIDTH WCONTINUED
WEOF WEXITED WEXITSTATUS WIFCONTINUED WIFEXITED WIFSIGNALED WIFSTOPPED WINT_MAX
WINT_MIN WINT_WIDTH WNOHANG WNOWAIT WORD_BIT WSTOPPED WSTOPSIG WTERMSIG
WUNTRACED W_OK XATTR_LIST_MAX XATTR_NAME_MAX XATTR_SIZE_MAX X_OK alloca be16toh
be32toh be64toh errno htobe16 htobe32 htobe64 htole16 htole32
htole64 le16toh le32toh le64toh linux offsetof pthread_cleanup_pop pthread_cleanup_pop_restore_np
pthread_cleanup_push pthread_cleanup_push_defer_np sched_priority stderr stdin stdout strdupa strndupa
unix
`
