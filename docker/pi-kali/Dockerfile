ARG KALI_BASE_IMAGE=kalilinux/kali-last-release@sha256:3ea545e38849417fc933514e117014b98aa42fd87d239177fa4c9874dcb73d5f
FROM ${KALI_BASE_IMAGE}

ARG DEBIAN_FRONTEND=noninteractive
ARG PI_VERSION=1.0.4

RUN find /etc/apt -type f \( -name '*.list' -o -name '*.sources' \) \
        -exec sed -i 's/kali-rolling/kali-last-snapshot/g' {} + \
    && grep -Rqs 'kali-last-snapshot' /etc/apt \
    && apt-get update \
    && apt-get install -y --no-install-recommends \
        kali-linux-core \
        ca-certificates \
        curl \
        wget \
        git \
        jq \
        ripgrep \
        file \
        procps \
        iproute2 \
        iputils-ping \
        dnsutils \
        whois \
        netcat-openbsd \
        socat \
        openssh-client \
        nmap \
        masscan \
        tcpdump \
        ffuf \
        gobuster \
        nikto \
        whatweb \
        sqlmap \
        hydra \
        binutils \
        gdb \
        strace \
        ltrace \
        gcc \
        make \
        python3 \
        python3-pip \
        python3-venv \
        nodejs \
        npm \
    && node -e 'const [M,m]=process.versions.node.split(".").map(Number); if (M < 22 || (M === 22 && m < 19)) { console.error("Pi requires Node >= 22.19"); process.exit(1) }' \
    && npm install -g --ignore-scripts "@earendil-works/pi-coding-agent@${PI_VERSION}" \
    && pi --version \
    && apt-get clean \
    && rm -rf /var/lib/apt/lists/* /root/.npm

ENV PI_TELEMETRY=0 \
    PI_SKIP_VERSION_CHECK=1

WORKDIR /run/harness

CMD ["pi", "--version"]
