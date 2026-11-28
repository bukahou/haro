// 界面偏好存的键名，和开页前就套上外观的那段脚本（在 React 接手之前跑，免得选了深色的人先看到一闪浅色）。
// 不是 "use client" 模块：根布局（服务端组件）要拿到真的字符串。
export const uiStorageKey = "haro-ui";

export const themeScript = `try{var s=JSON.parse(localStorage.getItem(${JSON.stringify(uiStorageKey)})||"{}").state;if(s&&(s.theme==="light"||s.theme==="dark"))document.documentElement.dataset.theme=s.theme}catch(e){}`;
