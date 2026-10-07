FROM kalilinux/kali-rolling

ARG DEBIAN_FRONTEND=noninteractive
ARG PI_VERSION=1.0.4

RUN apt-get update \
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
