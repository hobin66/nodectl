{
  "presets": [
    {
      "name": "极简基础",
      "desc": "仅包含最核心的搜索引擎、通讯软件及 AI 工具分流。",
      "modules": ["Telegram", "Google", "GoogleFCM", "OpenAI", "GitHub", "Microsoft"]
    },
    {
      "name": "常用推荐 (默认)",
      "desc": "覆盖 90% 日常需求，包含主流流媒体、社媒与常用软件。",
      "modules": ["Telegram", "Google", "GoogleFCM", "YouTube", "Netflix", "Twitter(X)", "GitHub", "OpenAI", "Spotify", "Discord", "Microsoft", "TikTok"]
    },
    {
      "name": "高大全",
      "desc": "强迫症首选，启用所有支持的独立分流规则集。",
      "modules": ["ALL"]
    }
  ],
  "modules": [
    {
      "name": "小红书",
      "icon": "https://cdn.jsdelivr.net/gh/GitMetaio/Surfing@rm/Home/icon/XiaoHongShu.svg",
      "domain_url": "https://cdn.jsdelivr.net/gh/GitMetaio/rule@master/rule/Clash/XiaoHongShu/XiaoHongShu_OCD_Domain.mrs",
      "extra_rules": ["PROCESS-PATH,com.xingin.xhs"]
    },
    {
      "name": "抖音",
      "icon": "https://cdn.jsdelivr.net/gh/GitMetaio/Surfing@rm/Home/icon/DouYin.svg",
      "domain_url": "https://cdn.jsdelivr.net/gh/GitMetaio/rule@master/rule/Clash/DouYin/DouYin_OCD_Domain.mrs",
      "extra_rules": ["PROCESS-PATH,com.ss.android.ugc.aweme"]
    },
    {
      "name": "BiliBili",
      "icon": "https://cdn.jsdelivr.net/gh/GitMetaio/Surfing@rm/Home/icon/BiliBili.svg",
      "domain_url": "https://cdn.jsdelivr.net/gh/GitMetaio/rule@master/rule/Clash/BiliBili/BiliBili_OCD_Domain.mrs",
      "ip_url": "https://cdn.jsdelivr.net/gh/GitMetaio/rule@master/rule/Clash/BiliBili/BiliBili_OCD_IP.mrs",
      "extra_rules": ["PROCESS-PATH,tv.danmaku.bili"]
    },
    {
      "name": "Steam",
      "icon": "https://cdn.jsdelivr.net/gh/GitMetaio/Surfing@rm/Home/icon/Steam.svg",
      "domain_url": "https://cdn.jsdelivr.net/gh/GitMetaio/rule@master/rule/Clash/Steam/Steam_OCD_Domain.mrs"
    },
    {
      "name": "Apple",
      "icon": "https://cdn.jsdelivr.net/gh/GitMetaio/Surfing@rm/Home/icon/Apple.svg",
      "domain_url": "https://cdn.jsdelivr.net/gh/GitMetaio/rule@master/rule/Clash/Apple/Apple_OCD_Domain.mrs",
      "ip_url": "https://cdn.jsdelivr.net/gh/GitMetaio/rule@master/rule/Clash/Apple/Apple_OCD_IP.mrs"
    },
    {
      "name": "Microsoft",
      "icon": "https://cdn.jsdelivr.net/gh/GitMetaio/Surfing@rm/Home/icon/Microsoft.svg",
      "domain_url": "https://cdn.jsdelivr.net/gh/GitMetaio/rule@master/rule/Clash/Microsoft/Microsoft_OCD_Domain.mrs"
    },
    {
      "name": "Telegram",
      "icon": "https://cdn.jsdelivr.net/gh/GitMetaio/Surfing@rm/Home/icon/Telegram.svg",
      "domain_url": "https://cdn.jsdelivr.net/gh/GitMetaio/rule@master/rule/Clash/Telegram/Telegram_OCD_Domain.mrs",
      "ip_url": "https://cdn.jsdelivr.net/gh/GitMetaio/rule@master/rule/Clash/Telegram/Telegram_OCD_IP.mrs"
    },
    {
      "name": "Discord",
      "icon": "https://cdn.jsdelivr.net/gh/GitMetaio/Surfing@rm/Home/icon/Discord.svg",
      "domain_url": "https://cdn.jsdelivr.net/gh/GitMetaio/rule@master/rule/Clash/Discord/Discord_OCD_Domain.mrs"
    },
    {
      "name": "Spotify",
      "icon": "https://cdn.jsdelivr.net/gh/GitMetaio/Surfing@rm/Home/icon/Spotify.svg",
      "domain_url": "https://cdn.jsdelivr.net/gh/GitMetaio/rule@master/rule/Clash/Spotify/Spotify_OCD_Domain.mrs",
      "ip_url": "https://cdn.jsdelivr.net/gh/GitMetaio/rule@master/rule/Clash/Spotify/Spotify_OCD_IP.mrs"
    },
    {
      "name": "TikTok",
      "icon": "https://cdn.jsdelivr.net/gh/GitMetaio/Surfing@rm/Home/icon/TikTok.svg",
      "domain_url": "https://cdn.jsdelivr.net/gh/GitMetaio/rule@master/rule/Clash/TikTok/TikTok_OCD_Domain.mrs"
    },
    {
      "name": "加密货币",
      "icon": "https://cdn.jsdelivr.net/gh/homarr-labs/dashboard-icons/svg/bitcoin.svg",
      "domain_url": "https://cdn.jsdelivr.net/gh/MetaCubeX/meta-rules-dat@meta/geo/geosite/category-cryptocurrency.mrs"
    },
    {
      "name": "YouTube",
      "icon": "https://cdn.jsdelivr.net/gh/GitMetaio/Surfing@rm/Home/icon/YouTube.svg",
      "domain_url": "https://cdn.jsdelivr.net/gh/GitMetaio/rule@master/rule/Clash/YouTube/YouTube_OCD_Domain.mrs",
      "ip_url": "https://cdn.jsdelivr.net/gh/GitMetaio/rule@master/rule/Clash/YouTube/YouTube_OCD_IP.mrs"
    },
    {
      "name": "Netflix",
      "icon": "https://cdn.jsdelivr.net/gh/GitMetaio/Surfing@rm/Home/icon/Netflix.svg",
      "domain_url": "https://cdn.jsdelivr.net/gh/GitMetaio/rule@master/rule/Clash/Netflix/Netflix_OCD_Domain.mrs",
      "ip_url": "https://cdn.jsdelivr.net/gh/GitMetaio/rule@master/rule/Clash/Netflix/Netflix_OCD_IP.mrs"
    },
    {
      "name": "Google",
      "icon": "https://cdn.jsdelivr.net/gh/GitMetaio/Surfing@rm/Home/icon/Google.svg",
      "domain_url": "https://cdn.jsdelivr.net/gh/GitMetaio/rule@master/rule/Clash/Google/Google_OCD_Domain.mrs",
      "ip_url": "https://cdn.jsdelivr.net/gh/GitMetaio/rule@master/rule/Clash/Google/Google_OCD_IP.mrs"
    },
    {
      "name": "GoogleFCM",
      "icon": "https://cdn.jsdelivr.net/gh/GitMetaio/Surfing@rm/Home/icon/GoogleFCM.svg",
      "domain_url": "https://cdn.jsdelivr.net/gh/GitMetaio/rule@master/rule/Clash/GoogleFCM/GoogleFCM_OCD_Domain.mrs",
      "ip_url": "https://cdn.jsdelivr.net/gh/GitMetaio/rule@master/rule/Clash/GoogleFCM/GoogleFCM_OCD_IP.mrs",
      "extra_rules": ["DOMAIN-KEYWORD,mtalk.google"]
    },
    {
      "name": "Facebook",
      "icon": "https://cdn.jsdelivr.net/gh/GitMetaio/Surfing@rm/Home/icon/Facebook.svg",
      "domain_url": "https://cdn.jsdelivr.net/gh/GitMetaio/rule@master/rule/Clash/Facebook/Facebook_OCD_Domain.mrs",
      "ip_url": "https://cdn.jsdelivr.net/gh/GitMetaio/rule@master/rule/Clash/Facebook/Facebook_OCD_IP.mrs"
    },
    {
      "name": "OpenAI",
      "icon": "https://cdn.jsdelivr.net/gh/GitMetaio/Surfing@rm/Home/icon/OpenAI.svg",
      "domain_url": "https://cdn.jsdelivr.net/gh/GitMetaio/rule@master/rule/Clash/OpenAI/OpenAI_OCD_Domain.mrs",
      "ip_url": "https://cdn.jsdelivr.net/gh/GitMetaio/rule@master/rule/Clash/OpenAI/OpenAI_OCD_IP.mrs"
    },
    {
      "name": "GitHub",
      "icon": "https://cdn.jsdelivr.net/gh/GitMetaio/Surfing@rm/Home/icon/GitHub.svg",
      "domain_url": "https://cdn.jsdelivr.net/gh/GitMetaio/rule@master/rule/Clash/GitHub/GitHub_OCD_Domain.mrs"
    },
    {
      "name": "Twitter(X)",
      "icon": "https://cdn.jsdelivr.net/gh/GitMetaio/Surfing@rm/Home/icon/Twitter.svg",
      "domain_url": "https://cdn.jsdelivr.net/gh/GitMetaio/rule@master/rule/Clash/Twitter/Twitter_OCD_Domain.mrs",
      "ip_url": "https://cdn.jsdelivr.net/gh/GitMetaio/rule@master/rule/Clash/Twitter/Twitter_OCD_IP.mrs"
    },
    {
      "name": "秋风去广告",
      "icon": "https://awavenue.top/logo.png",
      "domain_url": "https://raw.githubusercontent.com/TG-Twilight/AWAvenue-Ads-Rule/main/Filters/AWAvenue-Ads-Rule-Clash.mrs",
      "type": "reject"
    }
  ]
}
