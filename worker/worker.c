/* Standalone bounded QuickJS worker. No quickjs-libc, module loader, or OS JS API. */
#include "quickjs.h"
#include "embedded.h"
#include <errno.h>
#include <fcntl.h>
#include <sys/socket.h>
#include <netinet/in.h>
#include <sys/wait.h>
#include <sys/mman.h>
#include <signal.h>
#include <stdint.h>
#include <stdio.h>
#include <stdlib.h>
#include <string.h>
#include <sys/resource.h>
#include <sys/time.h>
#include <time.h>
#include <unistd.h>
#ifdef __APPLE__
#include <sandbox.h>
#include <mach/mach.h>
#endif
#ifdef __linux__
#include <stddef.h>
#include <linux/audit.h>
#include <linux/filter.h>
#include <linux/seccomp.h>
#include <sys/prctl.h>
#include <sys/syscall.h>
#endif

#define REQUEST_MAX (2u * 1024u * 1024u)
#define RESPONSE_MAX (1024u * 1024u)
#define OUTPUT_MAX (64u * 1024u)
#define HEAP_MAX (64u * 1024u * 1024u)
static int64_t deadline;
static unsigned long long process_as_limit;
static int64_t mono_ms(void) { struct timespec t; clock_gettime(CLOCK_MONOTONIC, &t); return (int64_t)t.tv_sec*1000+t.tv_nsec/1000000; }
static int interrupted(JSRuntime *rt, void *opaque) { (void)rt; (void)opaque; return mono_ms() >= deadline; }
static void timeout_handler(int sig) { (void)sig;
#ifdef __APPLE__
    struct mach_task_basic_info info; mach_msg_type_number_t count=MACH_TASK_BASIC_INFO_COUNT;
    if(task_info(mach_task_self(),MACH_TASK_BASIC_INFO,(task_info_t)&info,&count)!=KERN_SUCCESS || info.resident_size>128u*1024u*1024u) {
        const char memory_error[]="{\"error\":{\"code\":\"SCRIPT_LIMIT\",\"message\":\"Worker process memory limit exceeded\"}}\n";
        write(STDOUT_FILENO,memory_error,sizeof(memory_error)-1);_exit(126);
    }
#endif
    if(mono_ms()<deadline)return;
    const char msg[]="{\"error\":{\"code\":\"SCRIPT_TIMEOUT\",\"message\":\"Script deadline exceeded\"}}\n"; write(STDOUT_FILENO,msg,sizeof(msg)-1); _exit(124); }
static int limit(int resource, rlim_t n) { struct rlimit r={n,n}; int rc=setrlimit(resource,&r); if(rc)fprintf(stderr,"setrlimit(%d): %s\n",resource,strerror(errno)); return rc; }
static int isolate(void) {
    if (limit(RLIMIT_CORE,0) || limit(RLIMIT_CPU,1) || limit(RLIMIT_FSIZE,0) || limit(RLIMIT_STACK,8u*1024u*1024u)) return -1;
#ifdef __APPLE__
    /* Darwin rejects finite AS/DATA limits. QuickJS has a hard allocator cap;
       the periodic watchdog separately enforces a sampled whole-process RSS cap. */
    char *error=NULL;
    const char *profile="(version 1)(deny default)(allow sysctl-read)(allow process-info* (target self))";
    int rc=sandbox_init(profile,0,&error);
    if(error) { fprintf(stderr,"sandbox_init: %s\n",error); sandbox_free_error(error); }
    return rc ? rc : limit(RLIMIT_NOFILE,16);
#elif defined(__linux__)
    if(limit(RLIMIT_NOFILE,16) || limit(RLIMIT_AS,256u*1024u*1024u)) return -1;
    struct rlimit actual_as;
    if(getrlimit(RLIMIT_AS,&actual_as) || actual_as.rlim_cur!=256u*1024u*1024u || actual_as.rlim_max!=256u*1024u*1024u)return -1;
    process_as_limit=(unsigned long long)actual_as.rlim_cur;
#if defined(__x86_64__)
#define WORKER_ARCH AUDIT_ARCH_X86_64
#elif defined(__aarch64__)
#define WORKER_ARCH AUDIT_ARCH_AARCH64
#else
#error Unsupported Linux architecture: fail closed
#endif
#define ALLOW(n) BPF_JUMP(BPF_JMP|BPF_JEQ|BPF_K,__NR_##n,0,1), BPF_STMT(BPF_RET|BPF_K,SECCOMP_RET_ALLOW)
    struct sock_filter filter[]={
        BPF_STMT(BPF_LD|BPF_W|BPF_ABS,offsetof(struct seccomp_data,arch)),
        BPF_JUMP(BPF_JMP|BPF_JEQ|BPF_K,WORKER_ARCH,1,0),
        BPF_STMT(BPF_RET|BPF_K,SECCOMP_RET_KILL_PROCESS),
        BPF_STMT(BPF_LD|BPF_W|BPF_ABS,offsetof(struct seccomp_data,nr)),
        ALLOW(read), ALLOW(write), ALLOW(close), ALLOW(brk), ALLOW(mmap), ALLOW(munmap), ALLOW(mremap),
        ALLOW(madvise), ALLOW(mprotect), ALLOW(clock_gettime), ALLOW(rt_sigreturn), ALLOW(exit), ALLOW(exit_group),
        BPF_STMT(BPF_RET|BPF_K,SECCOMP_RET_ERRNO|EPERM)
    };
    struct sock_fprog program={(unsigned short)(sizeof(filter)/sizeof(filter[0])),filter};
    if(prctl(PR_SET_NO_NEW_PRIVS,1,0,0,0) || prctl(PR_GET_NO_NEW_PRIVS,0,0,0,0)!=1) return -1;
    return prctl(PR_SET_SECCOMP,SECCOMP_MODE_FILTER,&program);
#else
    return -1;
#endif
}
/* Bounded buffering retains any bytes after a frame's newline. */
static char *read_line(size_t max,size_t *len) {
    static char pending[4096]; static size_t pos=0,used=0;
    char *buf=malloc(max+1); if(!buf)return NULL; size_t n=0;
    for(;;) {
        if(pos==used) { ssize_t r=read(STDIN_FILENO,pending,sizeof(pending));
            if(r<0&&errno==EINTR)continue;
            if(r<=0){free(buf);return NULL;}pos=0;used=(size_t)r;
        }
        char c=pending[pos++];if(c=='\n')break;
        if(n==max){free(buf);return NULL;}buf[n++]=c;
    }
    buf[n]=0;*len=n;return buf;
}
static int write_all(const char *buf,size_t n) { while(n) { ssize_t w=write(STDOUT_FILENO,buf,n); if(w<0&&errno==EINTR)continue; if(w<=0)return -1; buf+=w;n-=(size_t)w;} return 0; }
static int emit(JSContext *ctx,JSValueConst value) {
    JSValue json=JS_JSONStringify(ctx,value,JS_UNDEFINED,JS_UNDEFINED);
    if(JS_IsException(json))return -1;
    size_t n; const char *s=JS_ToCStringLen(ctx,&n,json);
    int rc=(!s||n>OUTPUT_MAX)?-1:write_all(s,n);
    if(!rc)rc=write_all("\n",1);
    if(s)JS_FreeCString(ctx,s); JS_FreeValue(ctx,json); return rc;
}
static JSValue host_read(JSContext *ctx,JSValueConst this_val,int argc,JSValueConst *argv) {
    (void)this_val;
    if(argc!=1 || !JS_IsString(argv[0])) return JS_ThrowTypeError(ctx,"Invalid factor reference");
    JSValue msg=JS_NewObject(ctx); JS_SetPropertyStr(ctx,msg,"read",JS_DupValue(ctx,argv[0]));
    int rc=emit(ctx,msg); JS_FreeValue(ctx,msg);
    if(rc)return JS_ThrowInternalError(ctx,"Cannot emit factor request");
    size_t n;char *line=read_line(RESPONSE_MAX,&n);
    if(!line)return JS_ThrowInternalError(ctx,"Missing or oversized factor response");
    JSValue response=JS_ParseJSON(ctx,line,n,"<factor-response>");free(line);return response;
}
static void literal_error(const char *code,const char *message) {
    char buf[512]; int n=snprintf(buf,sizeof(buf),"{\"error\":{\"code\":\"%s\",\"message\":\"%s\"}}\n",code,message);
    if(n>0 && (size_t)n<sizeof(buf))write_all(buf,(size_t)n);
}
static void emit_exception(JSContext *ctx) {
    JSValue ex=JS_GetException(ctx),err=JS_NewObject(ctx),out=JS_NewObject(ctx);
    JSValue code=JS_GetPropertyStr(ctx,ex,"code");
    if(!JS_IsString(code)) { JS_FreeValue(ctx,code);code=JS_NewString(ctx,mono_ms()>=deadline?"SCRIPT_TIMEOUT":"SCRIPT_ERROR"); }
    JSValue message=JS_GetPropertyStr(ctx,ex,"message");
    if(!JS_IsString(message)) {JS_FreeValue(ctx,message);message=JS_ToString(ctx,ex);}
    JS_SetPropertyStr(ctx,err,"code",code); JS_SetPropertyStr(ctx,err,"message",message);
    const char *keys[]={"line","column"};
    for(int i=0;i<2;i++) { JSValue p=JS_GetPropertyStr(ctx,ex,keys[i]); if(JS_IsNumber(p))JS_SetPropertyStr(ctx,err,keys[i],p);else JS_FreeValue(ctx,p); }
    JS_SetPropertyStr(ctx,out,"error",err);
    if(emit(ctx,out))literal_error("SCRIPT_LIMIT","Worker memory or output limit exceeded");
    JS_FreeValue(ctx,out);JS_FreeValue(ctx,ex);
}
static int selftest_isolation(void) {
    int fd=open("/etc/passwd",O_RDONLY);int file_denied=fd<0&&(errno==EPERM||errno==EACCES);if(fd>=0)close(fd);
    fd=socket(AF_INET,SOCK_STREAM,0);int socket_denied=fd<0&&(errno==EPERM||errno==EACCES);
    if(fd>=0){struct sockaddr_in address;memset(&address,0,sizeof(address));address.sin_family=AF_INET;address.sin_port=htons(9);address.sin_addr.s_addr=htonl(INADDR_LOOPBACK);
        int rc=connect(fd,(struct sockaddr*)&address,sizeof(address));socket_denied=rc<0&&(errno==EPERM||errno==EACCES);close(fd);
    }
    pid_t child=fork();int fork_denied=child<0&&(errno==EPERM||errno==EACCES);if(child==0)_exit(0);if(child>0)waitpid(child,NULL,0);
    if(!file_denied||!socket_denied||!fork_denied){literal_error("SCRIPT_UNAVAILABLE","OS sandbox denial probe failed");return 125;}
    #ifdef __linux__
    void *large=mmap(NULL,257u*1024u*1024u,PROT_READ|PROT_WRITE,MAP_PRIVATE|MAP_ANONYMOUS,-1,0);
    if(large!=MAP_FAILED || errno!=ENOMEM){literal_error("SCRIPT_UNAVAILABLE","Hard address space limit probe failed");return 125;}
#endif
    char proof[512];int n=snprintf(proof,sizeof(proof),"{\"filesystemDenied\":true,\"networkDenied\":true,\"processCreationDenied\":true,\"heapLimitBytes\":%u,\"processAddressSpaceLimitBytes\":%llu,\"processRSSSampleLimitBytes\":%u}\n",HEAP_MAX,process_as_limit,
#ifdef __APPLE__
        128u*1024u*1024u
#else
        0u
#endif
    );
    if(n<0||(size_t)n>=sizeof(proof))return 125;write_all(proof,(size_t)n);
    return 0;
}
int main(int argc,char **argv) {
    deadline=mono_ms()+900;
    struct sigaction sa;memset(&sa,0,sizeof(sa));sa.sa_handler=timeout_handler;sigemptyset(&sa.sa_mask);
    if(sigaction(SIGALRM,&sa,NULL))return 125;
    struct itimerval timer={{0,10000},{0,10000}};
    if(setitimer(ITIMER_REAL,&timer,NULL))return 125;
    /* Close inherited descriptors before lowering RLIMIT_NOFILE. */
    #if defined(__linux__) && defined(__NR_close_range)
    if(syscall(__NR_close_range,3,~0U,0)<0) {
        long maxfd=sysconf(_SC_OPEN_MAX);if(maxfd<0)maxfd=1048576;
        for(int fd=3;fd<maxfd;fd++)close(fd);
    }
#else
    long maxfd=sysconf(_SC_OPEN_MAX);if(maxfd<0)maxfd=1048576;
    for(int fd=3;fd<maxfd;fd++)close(fd);
#endif
    if(isolate()) {literal_error("SCRIPT_UNAVAILABLE","OS sandbox initialization failed");return 125;}
    if(argc==2 && !strcmp(argv[1],"--selftest-isolation"))return selftest_isolation();
    JSRuntime *rt=JS_NewRuntime();if(!rt){literal_error("SCRIPT_LIMIT","Runtime allocation failed");return 1;}
    JS_SetMemoryLimit(rt,HEAP_MAX);JS_SetMaxStackSize(rt,512u*1024u);JS_SetInterruptHandler(rt,interrupted,NULL);
    JSContext *ctx=JS_NewContext(rt);if(!ctx){literal_error("SCRIPT_LIMIT","Context allocation failed");JS_FreeRuntime(rt);return 1;}
    JSValue global=JS_GetGlobalObject(ctx);
    JS_SetPropertyStr(ctx,global,"__hostRead",JS_NewCFunction(ctx,host_read,"__hostRead",1));
    JSValue result=JS_Eval(ctx,(const char*)acorn_js,acorn_js_len,"<acorn>",JS_EVAL_TYPE_GLOBAL);
    if(JS_IsException(result))goto failed;JS_FreeValue(ctx,result);
    result=JS_Eval(ctx,(const char*)bootstrap_js,bootstrap_js_len,"<bootstrap>",JS_EVAL_TYPE_GLOBAL);
    if(JS_IsException(result))goto failed;JS_FreeValue(ctx,result);
    size_t n;char *line=read_line(REQUEST_MAX,&n);
    if(!line){literal_error("INVALID_REQUEST","Missing or oversized worker request");goto done;}
    JSValue request=JS_ParseJSON(ctx,line,n,"<request>");free(line);
    if(JS_IsException(request))goto failed;
    JSValue run=JS_GetPropertyStr(ctx,global,"__run");
    result=JS_Call(ctx,run,JS_UNDEFINED,1,&request);JS_FreeValue(ctx,request);JS_FreeValue(ctx,run);
    if(JS_IsException(result))goto failed;
    if(emit(ctx,result))literal_error("SCRIPT_LIMIT","Worker output limit exceeded");
    JS_FreeValue(ctx,result);goto done;
failed: emit_exception(ctx);
done: JS_FreeValue(ctx,global);JS_FreeContext(ctx);JS_FreeRuntime(rt);return 0;
}
