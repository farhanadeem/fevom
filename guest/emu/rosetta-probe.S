.global _start
.text
_start:
    xor %r12d, %r12d
    mov $257, %eax
    mov $-100, %edi
    lea root(%rip), %rsi
    mov $0x200000, %edx
    xor %r10d, %r10d
    syscall
    test %rax, %rax
    js access_failed
    mov %eax, %edi
    mov $439, %eax
    lea empty(%rip), %rsi
    mov $1, %edx
    mov $0x1000, %r10d
    syscall
    test %rax, %rax
    jz access_done
access_failed:
    or $1, %r12d
access_done:
    mov $157, %eax
    mov $38, %edi
    mov $1, %esi
    xor %edx, %edx
    xor %r10d, %r10d
    xor %r8d, %r8d
    syscall
    test %rax, %rax
    jnz seccomp_failed
    mov $317, %eax
    mov $1, %edi
    xor %esi, %esi
    lea prog(%rip), %rdx
    syscall
    test %rax, %rax
    jnz seccomp_failed
    mov $110, %eax
    syscall
    cmp $-1, %rax
    je done
seccomp_failed:
    or $2, %r12d
done:
    mov %r12d, %edi
    or $64, %edi
    mov $60, %eax
    syscall
.section .rodata
root: .asciz "/"
empty: .asciz ""
.balign 8
prog:
    .short 7
    .zero 6
    .quad filter
filter:
    .short 0x20; .byte 0, 0; .long 4
    .short 0x15; .byte 1, 0; .long 0xc000003e
    .short 0x06; .byte 0, 0; .long 0x80000000
    .short 0x20; .byte 0, 0; .long 0
    .short 0x15; .byte 0, 1; .long 110
    .short 0x06; .byte 0, 0; .long 0x00050001
    .short 0x06; .byte 0, 0; .long 0x7fff0000
.section .note.GNU-stack,"",@progbits
