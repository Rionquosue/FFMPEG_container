#define _GNU_SOURCE
#include <errno.h>
#include <fcntl.h>
#include <linux/audit.h>
#include <linux/filter.h>
#include <linux/landlock.h>
#include <linux/seccomp.h>
#include <sched.h>
#include <stddef.h>
#include <stdint.h>
#include <stdio.h>
#include <stdlib.h>
#include <string.h>
#include <sys/prctl.h>
#include <sys/resource.h>
#include <sys/socket.h>
#include <sys/stat.h>
#include <sys/syscall.h>
#include <unistd.h>

/* No privileged setup, namespace creation, shell or fallback path. */
static void die(const char *s) { fprintf(stderr, "sandbox: %s: %s\n", s, strerror(errno)); exit(125); }
static void limit(int which, rlim_t n) { struct rlimit r={n,n}; if(setrlimit(which,&r))die("rlimit"); }
static void rule(int ruleset,const char *path,uint64_t access,int optional) {
 int fd=open(path,O_PATH|O_CLOEXEC);
 if(fd<0){if(optional&&errno==ENOENT)return;die("open rule");}
 struct landlock_path_beneath_attr p={.allowed_access=access,.parent_fd=fd};
 if(syscall(SYS_landlock_add_rule,ruleset,LANDLOCK_RULE_PATH_BENEATH,&p,0))die("landlock rule");
 close(fd);
}

#if defined(__x86_64__)
#define ARCH AUDIT_ARCH_X86_64
#elif defined(__aarch64__)
#define ARCH AUDIT_ARCH_AARCH64
#else
#error Only amd64 and arm64 are supported
#endif
#define DENY(nr) BPF_JUMP(BPF_JMP|BPF_JEQ|BPF_K,(nr),0,1), BPF_STMT(BPF_RET|BPF_K,SECCOMP_RET_ERRNO|EPERM)
#define NOSYS(nr) BPF_JUMP(BPF_JMP|BPF_JEQ|BPF_K,(nr),0,1), BPF_STMT(BPF_RET|BPF_K,SECCOMP_RET_ERRNO|ENOSYS)

static void install(const struct sock_filter *code,size_t count) {
 struct sock_fprog program={.len=(unsigned short)count,.filter=(struct sock_filter *)code};
 if(prctl(PR_SET_NO_NEW_PRIVS,1,0,0,0))die("no_new_privs");
 if(prctl(PR_SET_SECCOMP,SECCOMP_MODE_FILTER,&program))die("seccomp");
}

/* Whole API may listen/respond but cannot initiate connections or send datagrams.
   Only INET TCP sockets are available. Inherited by every child. */
static void api_filter(void) {
 struct sock_filter f[]={
 BPF_STMT(BPF_LD|BPF_W|BPF_ABS,offsetof(struct seccomp_data,arch)),
 BPF_JUMP(BPF_JMP|BPF_JEQ|BPF_K,ARCH,1,0),BPF_STMT(BPF_RET|BPF_K,SECCOMP_RET_KILL_PROCESS),
 BPF_STMT(BPF_LD|BPF_W|BPF_ABS,offsetof(struct seccomp_data,nr)),
 #if defined(__x86_64__)
 BPF_JUMP(BPF_JMP|BPF_JGE|BPF_K,0x40000000,0,1),BPF_STMT(BPF_RET|BPF_K,SECCOMP_RET_KILL_PROCESS),
 #endif
 DENY(SYS_connect),DENY(SYS_sendto),DENY(SYS_sendmsg),DENY(SYS_sendmmsg),DENY(SYS_socketpair),
 DENY(SYS_io_uring_setup),DENY(SYS_io_uring_enter),DENY(SYS_io_uring_register),
 BPF_JUMP(BPF_JMP|BPF_JEQ|BPF_K,SYS_socket,0,8),
 BPF_STMT(BPF_LD|BPF_W|BPF_ABS,offsetof(struct seccomp_data,args[0])),
 BPF_JUMP(BPF_JMP|BPF_JEQ|BPF_K,AF_INET,2,0),
 BPF_JUMP(BPF_JMP|BPF_JEQ|BPF_K,AF_INET6,1,0),
 BPF_STMT(BPF_RET|BPF_K,SECCOMP_RET_ERRNO|EPERM),
 BPF_STMT(BPF_LD|BPF_W|BPF_ABS,offsetof(struct seccomp_data,args[1])),
 BPF_STMT(BPF_ALU|BPF_AND|BPF_K,0xf),
 BPF_JUMP(BPF_JMP|BPF_JEQ|BPF_K,SOCK_STREAM,1,0),
 BPF_STMT(BPF_RET|BPF_K,SECCOMP_RET_ERRNO|EPERM),
 BPF_STMT(BPF_RET|BPF_K,SECCOMP_RET_ALLOW)};
 install(f,sizeof(f)/sizeof(f[0]));
}

static void job_filter(void) {
 struct sock_filter f[]={
 BPF_STMT(BPF_LD|BPF_W|BPF_ABS,offsetof(struct seccomp_data,arch)),
 BPF_JUMP(BPF_JMP|BPF_JEQ|BPF_K,ARCH,1,0),BPF_STMT(BPF_RET|BPF_K,SECCOMP_RET_KILL_PROCESS),
 BPF_STMT(BPF_LD|BPF_W|BPF_ABS,offsetof(struct seccomp_data,nr)),
 #if defined(__x86_64__)
 BPF_JUMP(BPF_JMP|BPF_JGE|BPF_K,0x40000000,0,1),BPF_STMT(BPF_RET|BPF_K,SECCOMP_RET_KILL_PROCESS),
 DENY(SYS_fork),DENY(SYS_vfork),
 #endif
 DENY(SYS_socket),DENY(SYS_socketpair),DENY(SYS_connect),
 DENY(SYS_ptrace),DENY(SYS_process_vm_readv),DENY(SYS_process_vm_writev),
 DENY(SYS_pidfd_open),DENY(SYS_pidfd_getfd),DENY(SYS_pidfd_send_signal),
 DENY(SYS_kill),DENY(SYS_tkill),DENY(SYS_tgkill),DENY(SYS_rt_sigqueueinfo),DENY(SYS_rt_tgsigqueueinfo),
 DENY(SYS_setpgid),DENY(SYS_setsid),DENY(SYS_unshare),DENY(SYS_setns),
 DENY(SYS_mount),DENY(SYS_umount2),DENY(SYS_pivot_root),DENY(SYS_chroot),
 #ifdef SYS_chmod
 DENY(SYS_chmod),
 #endif
 #ifdef SYS_chown
 DENY(SYS_chown),
 #endif
 #ifdef SYS_lchown
 DENY(SYS_lchown),
 #endif
 DENY(SYS_fchmod),DENY(SYS_fchmodat),DENY(SYS_fchown),DENY(SYS_fchownat),
 #ifdef SYS_fchmodat2
 DENY(SYS_fchmodat2),
 #endif
 DENY(SYS_bpf),DENY(SYS_perf_event_open),DENY(SYS_userfaultfd),
 DENY(SYS_prlimit64),DENY(SYS_setrlimit),DENY(SYS_setpriority),
 DENY(SYS_sched_setparam),DENY(SYS_sched_setscheduler),DENY(SYS_sched_setaffinity),
 DENY(SYS_io_uring_setup),DENY(SYS_io_uring_enter),DENY(SYS_io_uring_register),
 NOSYS(SYS_clone3),
 BPF_JUMP(BPF_JMP|BPF_JEQ|BPF_K,SYS_clone,0,4),
 BPF_STMT(BPF_LD|BPF_W|BPF_ABS,offsetof(struct seccomp_data,args[0])),
 BPF_STMT(BPF_ALU|BPF_AND|BPF_K,CLONE_THREAD),
 BPF_JUMP(BPF_JMP|BPF_JEQ|BPF_K,CLONE_THREAD,1,0),
 BPF_STMT(BPF_RET|BPF_K,SECCOMP_RET_ERRNO|EPERM),
 BPF_STMT(BPF_RET|BPF_K,SECCOMP_RET_ALLOW)};
 install(f,sizeof(f)/sizeof(f[0]));
}

static void confine(const char *dir,rlim_t max_file,rlim_t cpu) {
 int abi=syscall(SYS_landlock_create_ruleset,NULL,0,LANDLOCK_CREATE_RULESET_VERSION);
 if(abi<3){errno=ENOTSUP;die("Landlock ABI 3 or newer required");}
 struct landlock_ruleset_attr attr={.handled_access_fs=(1ULL<<15)-1};
 int ruleset=syscall(SYS_landlock_create_ruleset,&attr,sizeof(attr),0);
 if(ruleset<0)die("landlock create");
 uint64_t read=LANDLOCK_ACCESS_FS_READ_FILE|LANDLOCK_ACCESS_FS_READ_DIR;
 rule(ruleset,"/usr/lib",read,0);rule(ruleset,"/lib",read,0);
 rule(ruleset,"/lib64",read,1);rule(ruleset,"/etc/ld.so.cache",LANDLOCK_ACCESS_FS_READ_FILE,1);
 rule(ruleset,"/usr/bin/ffmpeg",LANDLOCK_ACCESS_FS_READ_FILE|LANDLOCK_ACCESS_FS_EXECUTE,0);
 rule(ruleset,"/usr/bin/ffprobe",LANDLOCK_ACCESS_FS_READ_FILE|LANDLOCK_ACCESS_FS_EXECUTE,0);
 char path[4096];
 if(snprintf(path,sizeof(path),"%s/input",dir)>=(int)sizeof(path))die("path length");
 rule(ruleset,path,LANDLOCK_ACCESS_FS_READ_FILE,0);
 if(snprintf(path,sizeof(path),"%s/output",dir)>=(int)sizeof(path))die("path length");
 rule(ruleset,path,LANDLOCK_ACCESS_FS_READ_FILE|LANDLOCK_ACCESS_FS_WRITE_FILE|LANDLOCK_ACCESS_FS_TRUNCATE,0);
 if(prctl(PR_SET_NO_NEW_PRIVS,1,0,0,0))die("no_new_privs");
 if(syscall(SYS_landlock_restrict_self,ruleset,0))die("landlock restrict");
 close(ruleset);
 limit(RLIMIT_CORE,0);limit(RLIMIT_FSIZE,max_file);limit(RLIMIT_CPU,cpu);
 limit(RLIMIT_AS,768UL*1024*1024);limit(RLIMIT_NOFILE,64);
 job_filter();
}

int main(int argc,char **argv) {
 if(argc==2&&!strcmp(argv[1],"--check-egress")) {
  api_filter();
  int fd=socket(AF_INET,SOCK_STREAM,0);if(fd<0)die("TCP socket");
  if(connect(fd,NULL,0)!=-1||errno!=EPERM)die("egress test");
  close(fd);
  if(socket(AF_INET,SOCK_DGRAM,0)!=-1||errno!=EPERM)die("datagram test");
  if(socket(AF_UNIX,SOCK_STREAM,0)!=-1||errno!=EPERM)die("unix socket test");
  puts("egress denied");return 0;
 }
 if(argc==3&&!strcmp(argv[1],"--serve")) {
  if(strcmp(argv[2],"/usr/local/bin/ffmpeg-api")){errno=EINVAL;die("invalid server");}
  api_filter();execl(argv[2],argv[2],(char *)NULL);die("exec server");
 }
 if(argc==5&&!strcmp(argv[1],"--selftest")) {
  confine(argv[2],1024*1024,5);
  for(int i=3;i<5;i++){int fd=open(argv[i],O_RDONLY);if(fd>=0){close(fd);errno=EPERM;die("forbidden file readable");}if(errno!=EACCES)die("unexpected file test");}
  if(socket(AF_INET,SOCK_STREAM,0)!=-1||errno!=EPERM)die("network test");
  if(syscall(SYS_ptrace,0,0,0,0)!=-1||errno!=EPERM)die("ptrace test");
  puts("confined");return 0;
 }
 if(argc<6||strcmp(argv[1],"--job")){fprintf(stderr,"usage: media-sandbox --job DIR MAX_BYTES CPU_SECONDS ffmpeg|ffprobe [args]\n");return 125;}
 char *end;unsigned long long bytes=strtoull(argv[3],&end,10);if(*end||bytes<1||bytes>(1ULL<<40))return 125;
 unsigned long seconds=strtoul(argv[4],&end,10);if(*end||seconds<1||seconds>86401)return 125;
 const char *exe=NULL;if(!strcmp(argv[5],"ffmpeg"))exe="/usr/bin/ffmpeg";if(!strcmp(argv[5],"ffprobe"))exe="/usr/bin/ffprobe";if(!exe)return 125;
 confine(argv[2],bytes,seconds);argv[5]=(char *)exe;execv(exe,&argv[5]);die("exec processor");
 return 125;
}
