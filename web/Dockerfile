# syntax=docker/dockerfile:1
# haro 的网页版（Next.js standalone，SSR）。构建阶段只在原生架构跑一次（产物是纯 JS，和 CPU 架构无关），
# 运行阶段按目标架构各出一份。基础镜像钉 digest，两处写一样的（Dependabot 一起改；它认不出 ARG 里的镜像）。

FROM --platform=$BUILDPLATFORM node:22-alpine@sha256:1314fb30ec37592d5845e5a160d522c0e680972aa534549940b8d73bde88481a AS build
WORKDIR /app
# 按锁文件装，不运行依赖的安装脚本（.npmrc 的 ignore-scripts=true；npm ci 也认它）
COPY package.json package-lock.json .npmrc ./
RUN npm ci --ignore-scripts
COPY . .
# App 接口的根地址在构建时写进页面：⛔ 没有默认值，没传时 next.config.ts 让构建失败（不悄悄连错的地址）
ARG NEXT_PUBLIC_API_URL
# 源码版本（CI 传入短 sha）：页面的 <meta name="build"> 里看得到
ARG NEXT_PUBLIC_BUILD_SHA=dev
ENV NEXT_PUBLIC_API_URL=${NEXT_PUBLIC_API_URL} NEXT_PUBLIC_BUILD_SHA=${NEXT_PUBLIC_BUILD_SHA} NEXT_TELEMETRY_DISABLED=1
RUN npm run build

FROM node:22-alpine@sha256:1314fb30ec37592d5845e5a160d522c0e680972aa534549940b8d73bde88481a
WORKDIR /app
# 用镜像自带的 node 用户运行（不用 root）；运行时不往 .next 里写东西
COPY --from=build --chown=node:node /app/.next/standalone ./
COPY --from=build --chown=node:node /app/.next/static ./.next/static
COPY --from=build --chown=node:node /app/public ./public
ENV NODE_ENV=production PORT=3000 HOSTNAME=0.0.0.0 NEXT_TELEMETRY_DISABLED=1
EXPOSE 3000
USER node
CMD ["node", "server.js"]
